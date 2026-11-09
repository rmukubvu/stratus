package contract

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRDSAndElastiCacheRunRealEngines proves that a DB instance and a cache
// cluster are real servers: the endpoints the API returns speak the
// PostgreSQL and Redis wire protocols, go away on delete and reset, and come
// back on the same port after a snapshot restore.
func TestRDSAndElastiCacheRunRealEngines(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	runAWS(t, h.baseURL, "rds", "create-db-instance",
		"--db-instance-identifier", "orders-db", "--db-instance-class", "db.t3.micro", "--engine", "postgres",
		"--master-username", "app", "--master-user-password", "secret123",
		"--db-name", "orders", "--output", "json")
	db := describeDBInstance(t, h.baseURL, "orders-db")
	if db.Status != "available" || db.Endpoint.Address == "" || db.Endpoint.Port == 0 {
		t.Fatalf("instance should be available with a real endpoint: %+v", db)
	}
	if db.DBName != "orders" || db.MasterUsername != "app" {
		t.Fatalf("instance should report its database and master user: %+v", db)
	}
	assertPostgresSpeaks(t, db.Endpoint.Address, db.Endpoint.Port, "app", "orders")

	runAWS(t, h.baseURL, "elasticache", "create-cache-cluster",
		"--cache-cluster-id", "sessions", "--engine", "redis", "--output", "json")
	cache := describeCacheCluster(t, h.baseURL, "sessions")
	if cache.Status != "available" || len(cache.Nodes) != 1 || cache.Nodes[0].Endpoint.Port == 0 {
		t.Fatalf("cluster should be available with one real node: %+v", cache)
	}
	assertRedisSpeaks(t, cache.Nodes[0].Endpoint.Address, cache.Nodes[0].Endpoint.Port)

	// Snapshot keeps the instances; reset takes them down with their state.
	mustStatus(t, http.MethodPost, control+"/snapshots", `{"name":"with-engines"}`, http.StatusCreated)
	reset := mustStatus(t, http.MethodPost, control+"/reset", `{"services":["rds"]}`, http.StatusOK)
	if !strings.Contains(reset, `"engines_stopped":["rds"]`) {
		t.Fatalf("reset should report stopped engines: %s", reset)
	}
	assertRefused(t, db.Endpoint.Address, db.Endpoint.Port)
	assertRedisSpeaks(t, cache.Nodes[0].Endpoint.Address, cache.Nodes[0].Endpoint.Port)
	if out := runAWS(t, h.baseURL, "rds", "describe-db-instances", "--output", "json"); strings.Contains(string(out), "orders-db") {
		t.Fatalf("instance should be gone after reset: %s", out)
	}

	// Restore brings every engine back on its recorded endpoint.
	mustStatus(t, http.MethodPost, control+"/snapshots/with-engines/restore", "", http.StatusOK)
	restored := describeDBInstance(t, h.baseURL, "orders-db")
	if restored.Endpoint.Port != db.Endpoint.Port || restored.Status != "available" {
		t.Fatalf("restored instance should keep its endpoint: before %+v after %+v", db.Endpoint, restored.Endpoint)
	}
	assertPostgresSpeaks(t, restored.Endpoint.Address, restored.Endpoint.Port, "app", "orders")
	assertRedisSpeaks(t, cache.Nodes[0].Endpoint.Address, cache.Nodes[0].Endpoint.Port)

	// Delete stops the engine for good.
	runAWS(t, h.baseURL, "rds", "delete-db-instance", "--db-instance-identifier", "orders-db", "--skip-final-snapshot", "--output", "json")
	runAWS(t, h.baseURL, "elasticache", "delete-cache-cluster", "--cache-cluster-id", "sessions", "--output", "json")
	assertRefused(t, db.Endpoint.Address, db.Endpoint.Port)
	assertRefused(t, cache.Nodes[0].Endpoint.Address, cache.Nodes[0].Endpoint.Port)
}

func TestRDSRejectsUnsupportedEngineBeforeStartingAnything(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	out := runAWSExpectingFailure(t, h.baseURL, "rds", "create-db-instance",
		"--db-instance-identifier", "legacy", "--db-instance-class", "db.t3.micro", "--engine", "oracle-ee",
		"--master-username", "app", "--master-user-password", "secret123")
	if !strings.Contains(out, "InvalidParameterValue") || !strings.Contains(out, "postgres") {
		t.Fatalf("error should reject the engine and list supported ones: %s", out)
	}
}

type dbInstance struct {
	Status         string `json:"DBInstanceStatus"`
	DBName         string `json:"DBName"`
	MasterUsername string `json:"MasterUsername"`
	Endpoint       struct {
		Address string `json:"Address"`
		Port    int    `json:"Port"`
	} `json:"Endpoint"`
}

func describeDBInstance(t *testing.T, endpoint, id string) dbInstance {
	t.Helper()
	out := runAWS(t, endpoint, "rds", "describe-db-instances", "--db-instance-identifier", id, "--output", "json")
	var payload struct {
		DBInstances []dbInstance `json:"DBInstances"`
	}
	if err := json.Unmarshal(out, &payload); err != nil || len(payload.DBInstances) != 1 {
		t.Fatalf("decode describe-db-instances: %v\n%s", err, out)
	}
	return payload.DBInstances[0]
}

type cacheCluster struct {
	Status string `json:"CacheClusterStatus"`
	Nodes  []struct {
		Endpoint struct {
			Address string `json:"Address"`
			Port    int    `json:"Port"`
		} `json:"Endpoint"`
	} `json:"CacheNodes"`
}

func describeCacheCluster(t *testing.T, endpoint, id string) cacheCluster {
	t.Helper()
	out := runAWS(t, endpoint, "elasticache", "describe-cache-clusters", "--cache-cluster-id", id, "--show-cache-node-info", "--output", "json")
	var payload struct {
		CacheClusters []cacheCluster `json:"CacheClusters"`
	}
	if err := json.Unmarshal(out, &payload); err != nil || len(payload.CacheClusters) != 1 {
		t.Fatalf("decode describe-cache-clusters: %v\n%s", err, out)
	}
	return payload.CacheClusters[0]
}

// assertPostgresSpeaks opens a connection and sends a protocol 3.0 startup
// message. A real PostgreSQL answers with an authentication request ('R'),
// which no port-forwarding stub would do.
func assertPostgresSpeaks(t *testing.T, address string, port int, user, database string) {
	t.Helper()
	conn := dialEngine(t, address, port)
	defer conn.Close()

	var params bytes.Buffer
	params.WriteString("user\x00" + user + "\x00database\x00" + database + "\x00\x00")
	msg := make([]byte, 8, 8+params.Len())
	binary.BigEndian.PutUint32(msg[0:4], uint32(8+params.Len()))
	binary.BigEndian.PutUint32(msg[4:8], 196608) // protocol 3.0
	msg = append(msg, params.Bytes()...)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write startup message: %v", err)
	}
	kind := make([]byte, 1)
	if _, err := conn.Read(kind); err != nil {
		t.Fatalf("read postgres reply: %v", err)
	}
	if kind[0] != 'R' {
		t.Fatalf("expected PostgreSQL authentication request, got message type %q", kind[0])
	}
}

// assertRedisSpeaks requires a +PONG to a RESP PING.
func assertRedisSpeaks(t *testing.T, address string, port int) {
	t.Helper()
	conn := dialEngine(t, address, port)
	defer conn.Close()
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "+PONG" {
		t.Fatalf("expected +PONG, got %q (%v)", line, err)
	}
}

func assertRefused(t *testing.T, address string, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", address, port), time.Second)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s:%d still accepts connections", address, port)
}

func dialEngine(t *testing.T, address string, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", address, port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s:%d: %v", address, port, err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

// runAWSExpectingFailure runs an AWS CLI command that must fail and returns
// its combined output.
func runAWSExpectingFailure(t *testing.T, endpoint string, args ...string) string {
	t.Helper()
	cmd := exec.Command("aws", append([]string{"--endpoint-url", endpoint}, args...)...)
	cmd.Env = awsEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("aws %s should have failed:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}
