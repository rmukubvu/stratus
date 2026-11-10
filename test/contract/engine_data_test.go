package contract

import (
	"bufio"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotsCaptureEngineData proves that a snapshot holds what is inside
// the engines, not just which engines exist: rows written to PostgreSQL and
// keys written to Valkey come back after a restore, later writes are undone,
// and a fresh Stratus seeded from the snapshot starts with the same data on
// its own ports.
func TestSnapshotsCaptureEngineData(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	runAWS(t, h.baseURL, "rds", "create-db-instance",
		"--db-instance-identifier", "orders-db", "--db-instance-class", "db.t3.micro", "--engine", "postgres",
		"--master-username", "app", "--master-user-password", "secret123", "--db-name", "orders", "--output", "json")
	runAWS(t, h.baseURL, "elasticache", "create-cache-cluster",
		"--cache-cluster-id", "sessions", "--engine", "redis", "--output", "json")
	db := describeDBInstance(t, h.baseURL, "orders-db")
	cache := describeCacheCluster(t, h.baseURL, "sessions").Nodes[0].Endpoint

	psql(t, "orders-db", "app", "orders", "create table orders (id int primary key); insert into orders values (1), (2);")
	redisDo(t, cache.Address, cache.Port, "SET", "session:42", "alice")

	created := mustStatus(t, http.MethodPost, control+"/snapshots", `{"name":"with-data"}`, http.StatusCreated)
	if !strings.Contains(created, "rds/orders-db") || !strings.Contains(created, "elasticache/sessions") {
		t.Fatalf("snapshot should list the engines it captured: %s", created)
	}

	// Change everything after the snapshot.
	psql(t, "orders-db", "app", "orders", "insert into orders values (3); create table scratch (x int);")
	redisDo(t, cache.Address, cache.Port, "SET", "session:42", "mallory")
	redisDo(t, cache.Address, cache.Port, "SET", "session:99", "eve")

	mustStatus(t, http.MethodPost, control+"/snapshots/with-data/restore", "", http.StatusOK)

	if got := psql(t, "orders-db", "app", "orders", "select string_agg(id::text, ',' order by id) from orders;"); got != "1,2" {
		t.Fatalf("restore should bring back the snapshot's rows, got %q", got)
	}
	if got := psql(t, "orders-db", "app", "orders", "select count(*) from pg_tables where tablename = 'scratch';"); got != "0" {
		t.Fatalf("a table created after the snapshot should be gone, got %q", got)
	}
	if got := redisDo(t, cache.Address, cache.Port, "GET", "session:42"); got != "alice" {
		t.Fatalf("restore should bring back the snapshot's keys, got %q", got)
	}
	if got := redisDo(t, cache.Address, cache.Port, "EXISTS", "session:99"); got != "0" {
		t.Fatalf("a key written after the snapshot should be gone, got %q", got)
	}
	if restored := describeDBInstance(t, h.baseURL, "orders-db"); restored.Endpoint.Port != db.Endpoint.Port {
		t.Fatalf("restore should keep the endpoint: before %d after %d", db.Endpoint.Port, restored.Endpoint.Port)
	}

	// A fork seeded from the snapshot runs beside the original with the same
	// data on its own ports.
	fork := startHarnessWithArgs(t, t.TempDir(), []string{"--seed", filepath.Join(h.dataDir, "snapshots", "with-data")})
	defer fork.Close()
	forkDB := describeDBInstance(t, fork.baseURL, "orders-db")
	forkCache := describeCacheCluster(t, fork.baseURL, "sessions").Nodes[0].Endpoint
	if forkDB.Status != "available" || forkDB.Endpoint.Port == db.Endpoint.Port || forkCache.Port == cache.Port {
		t.Fatalf("fork should run its own engines on new ports: db %+v cache %+v", forkDB, forkCache)
	}
	if got := redisDo(t, forkCache.Address, forkCache.Port, "GET", "session:42"); got != "alice" {
		t.Fatalf("fork should start from the snapshot's keys, got %q", got)
	}
	if got := psqlAt(t, forkDB.Endpoint.Port, "app", "secret123", "orders", "select count(*) from orders;"); got != "2" {
		t.Fatalf("fork should start from the snapshot's rows, got %q", got)
	}
}

// psql runs SQL inside the engine container of a DB instance and returns the
// unaligned, header-free result.
func psql(t *testing.T, instance, user, database, sql string) string {
	t.Helper()
	id := engineContainer(t, "rds", instance)
	out, err := exec.Command("docker", "exec", id, "psql", "-U", user, "-d", database, "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// psqlAt connects over the published port from a throwaway client
// container, proving the endpoint itself serves the data.
func psqlAt(t *testing.T, port int, user, password, database, sql string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--network", "host", "-e", "PGPASSWORD="+password,
		"postgres:16-alpine", "psql", "-h", "127.0.0.1", "-p", fmt.Sprint(port), "-U", user, "-d", database, "-tA", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql at port %d %q: %v\n%s", port, sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func engineContainer(t *testing.T, kind, resource string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q",
		"--filter", "label=io.stratus.kind="+kind, "--filter", "label=io.stratus.resource="+resource).Output()
	ids := strings.Fields(string(out))
	if err != nil || len(ids) == 0 {
		t.Fatalf("no running %s container for %s (%v)", kind, resource, err)
	}
	return ids[0]
}

// redisDo sends one command over RESP and returns a simple reply as text:
// the bulk string, integer or status, with "" for a nil bulk string.
func redisDo(t *testing.T, address string, port int, args ...string) string {
	t.Helper()
	conn := dialEngine(t, address, port)
	defer conn.Close()
	var cmd strings.Builder
	fmt.Fprintf(&cmd, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&cmd, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(cmd.String())); err != nil {
		t.Fatalf("write %v: %v", args, err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read reply to %v: %v", args, err)
	}
	line = strings.TrimRight(line, "\r\n")
	switch line[0] {
	case '+', ':':
		return line[1:]
	case '$':
		if line == "$-1" {
			return ""
		}
		value, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read bulk reply to %v: %v", args, err)
		}
		return strings.TrimRight(value, "\r\n")
	default:
		t.Fatalf("%v failed: %s", args, line)
		return ""
	}
}
