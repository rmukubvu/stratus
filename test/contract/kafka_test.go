package contract

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const redpandaImage = "redpandadata/redpanda:v24.2.7"

// TestMSKClustersRunRealBrokers proves that an MSK cluster is a real Kafka
// broker: records produced to the bootstrap address the API returns can be
// consumed again, the topic survives a snapshot restore, and delete takes the
// broker away.
func TestMSKClustersRunRealBrokers(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	out := runAWS(t, h.baseURL, "kafka", "create-cluster",
		"--cluster-name", "orders", "--kafka-version", "3.6.0", "--number-of-broker-nodes", "3",
		"--broker-node-group-info", `{"InstanceType":"kafka.m5.large","ClientSubnets":["subnet-1","subnet-2","subnet-3"]}`,
		"--output", "json")
	var created struct {
		ClusterArn string `json:"ClusterArn"`
		State      string `json:"State"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.State != "ACTIVE" {
		t.Fatalf("cluster should be active once created (%v): %s", err, out)
	}

	out = runAWS(t, h.baseURL, "kafka", "get-bootstrap-brokers", "--cluster-arn", created.ClusterArn, "--output", "json")
	var brokers struct {
		BootstrapBrokerString string `json:"BootstrapBrokerString"`
	}
	if err := json.Unmarshal(out, &brokers); err != nil || !strings.HasPrefix(brokers.BootstrapBrokerString, "127.0.0.1:") {
		t.Fatalf("bootstrap brokers should be a loopback address (%v): %s", err, out)
	}
	bootstrap := brokers.BootstrapBrokerString

	out = runAWS(t, h.baseURL, "kafka", "describe-cluster", "--cluster-arn", created.ClusterArn, "--output", "json")
	if !strings.Contains(string(out), `"NumberOfBrokerNodes": 3`) || !strings.Contains(string(out), "kafka.m5.large") {
		t.Fatalf("describe should echo the requested configuration: %s", out)
	}

	rpk(t, bootstrap, "topic", "create", "payments")
	rpkProduce(t, bootstrap, "payments", "order-1 paid")
	if got := rpkConsumeOne(t, bootstrap, "payments"); got != "order-1 paid" {
		t.Fatalf("consumed %q, want the produced record", got)
	}

	// The topic and its records are part of the snapshot.
	mustStatus(t, http.MethodPost, control+"/snapshots", `{"name":"with-topic"}`, http.StatusCreated)
	rpk(t, bootstrap, "topic", "delete", "payments")
	mustStatus(t, http.MethodPost, control+"/snapshots/with-topic/restore", "", http.StatusOK)
	if got := rpkConsumeOne(t, bootstrap, "payments"); got != "order-1 paid" {
		t.Fatalf("restore should bring the topic back with its records, got %q", got)
	}

	clusters := mustStatus(t, http.MethodGet, control+"/kafka/clusters", "", http.StatusOK)
	var view struct {
		Items []struct {
			Name             string `json:"name"`
			BootstrapBrokers string `json:"bootstrap_brokers"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(clusters), &view); err != nil || len(view.Items) != 1 || view.Items[0].BootstrapBrokers != bootstrap {
		t.Fatalf("control API should list the cluster with its bootstrap brokers (%v): %s", err, clusters)
	}

	runAWS(t, h.baseURL, "kafka", "delete-cluster", "--cluster-arn", created.ClusterArn, "--output", "json")
	host, port, _ := strings.Cut(bootstrap, ":")
	assertRefused(t, host, atoi(t, port))
}

func rpk(t *testing.T, bootstrap string, args ...string) string {
	t.Helper()
	full := append([]string{"run", "--rm", "--network", "host", "--entrypoint", "rpk", redpandaImage}, args...)
	full = append(full, "-X", "brokers="+bootstrap)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("rpk %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func rpkProduce(t *testing.T, bootstrap, topic, value string) {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "host", "--entrypoint", "rpk", redpandaImage,
		"topic", "produce", topic, "-X", "brokers="+bootstrap)
	cmd.Stdin = strings.NewReader(value + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("produce to %s: %v\n%s", topic, err, out)
	}
}

func rpkConsumeOne(t *testing.T, bootstrap, topic string) string {
	t.Helper()
	out := rpk(t, bootstrap, "topic", "consume", topic, "-n", "1", "-f", "%v")
	return strings.TrimSpace(out)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}
