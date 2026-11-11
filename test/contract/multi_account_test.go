package contract

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	accountA = "111111111111"
	accountB = "222222222222"
)

// runAWSAs runs the AWS CLI as a caller in account (the access key) and
// region, the convention Stratus uses to pick the account.
func runAWSAs(t *testing.T, endpoint, account, region string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("aws", append([]string{"--endpoint-url", endpoint, "--region", region, "--output", "json"}, args...)...)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+account,
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws %s as %s/%s failed: %v\n%s", strings.Join(args, " "), account, region, err, out)
	}
	return out
}

func queueURLs(t *testing.T, endpoint, account, region string) []string {
	t.Helper()
	var out struct {
		QueueUrls []string `json:"QueueUrls"`
	}
	raw := runAWSAs(t, endpoint, account, region, "sqs", "list-queues")
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode list-queues: %v\n%s", err, raw)
		}
	}
	return out.QueueUrls
}

func queueARN(t *testing.T, endpoint, account, region, url string) string {
	t.Helper()
	var out struct {
		Attributes map[string]string `json:"Attributes"`
	}
	raw := runAWSAs(t, endpoint, account, region, "sqs", "get-queue-attributes", "--queue-url", url, "--attribute-names", "QueueArn")
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode get-queue-attributes: %v\n%s", err, raw)
	}
	return out.Attributes["QueueArn"]
}

func TestMultiAccountCallerIdentityFollowsAccessKey(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	var id struct{ Account, Arn string }
	raw := runAWSAs(t, h.baseURL, accountA, "eu-west-1", "sts", "get-caller-identity")
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != accountA || !strings.Contains(id.Arn, accountA) {
		t.Fatalf("caller identity = %+v", id)
	}

	raw = runAWS(t, h.baseURL, "sts", "get-caller-identity", "--output", "json")
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != "000000000000" {
		t.Fatalf("a non-numeric key should keep the default account, got %+v", id)
	}
}

func TestMultiAccountRegionalResourcesAreIsolated(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	urlA := strings.Trim(string(runAWSAs(t, h.baseURL, accountA, "eu-central-1", "sqs", "create-queue", "--queue-name", "orders", "--query", "QueueUrl")), "\"\n ")
	if arn := queueARN(t, h.baseURL, accountA, "eu-central-1", urlA); arn != "arn:aws:sqs:eu-central-1:"+accountA+":orders" {
		t.Fatalf("queue ARN = %q", arn)
	}
	if !strings.Contains(urlA, "/"+accountA+"/orders") {
		t.Fatalf("queue URL should carry the account: %s", urlA)
	}

	// The same queue name is free in another account and another region.
	runAWSAs(t, h.baseURL, accountB, "eu-central-1", "sqs", "create-queue", "--queue-name", "orders")
	if got := queueURLs(t, h.baseURL, accountA, "us-west-2"); len(got) != 0 {
		t.Fatalf("account A in us-west-2 sees %v", got)
	}
	if got := queueURLs(t, h.baseURL, accountB, "eu-central-1"); len(got) != 1 {
		t.Fatalf("account B in eu-central-1 sees %v", got)
	}
	if got := queueURLs(t, h.baseURL, "test", "us-east-1"); len(got) != 0 {
		t.Fatalf("the default account sees %v", got)
	}

	// Scoped data survives a restart.
	h.Close()
	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()
	if got := queueURLs(t, h.baseURL, accountA, "eu-central-1"); len(got) != 1 || !strings.HasSuffix(got[0], "/orders") {
		t.Fatalf("account A's queue after restart: %v", got)
	}
}

func TestMultiAccountGlobalServicesSpanRegions(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWSAs(t, h.baseURL, accountA, "us-east-1", "s3api", "create-bucket", "--bucket", "landing-zone-logs")
	listA := string(runAWSAs(t, h.baseURL, accountA, "eu-west-1", "s3api", "list-buckets"))
	if !strings.Contains(listA, "landing-zone-logs") || !strings.Contains(listA, accountA) {
		t.Fatalf("account A should see its bucket from every region: %s", listA)
	}
	if listB := string(runAWSAs(t, h.baseURL, accountB, "us-east-1", "s3api", "list-buckets")); strings.Contains(listB, "landing-zone-logs") {
		t.Fatalf("account B sees account A's bucket: %s", listB)
	}

	role := string(runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-role",
		"--role-name", "deployer",
		"--assume-role-policy-document", `{"Version":"2012-10-17","Statement":[]}`))
	if !strings.Contains(role, "arn:aws:iam::"+accountA+":role/deployer") {
		t.Fatalf("role ARN should carry the account: %s", role)
	}
	if got := string(runAWSAs(t, h.baseURL, accountA, "ap-southeast-2", "iam", "get-role", "--role-name", "deployer")); !strings.Contains(got, "deployer") {
		t.Fatalf("IAM is global, the role should be visible from any region: %s", got)
	}
}

func TestMultiAccountSnapshotAndResetCoverEveryAccount(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	runAWSAs(t, h.baseURL, accountA, "eu-west-1", "sqs", "create-queue", "--queue-name", "kept")
	mustStatus(t, http.MethodPost, control+"/snapshots", `{"name":"accounts"}`, http.StatusCreated)
	runAWSAs(t, h.baseURL, accountA, "eu-west-1", "sqs", "create-queue", "--queue-name", "dropped")

	mustStatus(t, http.MethodPost, control+"/snapshots/accounts/restore", "", http.StatusOK)
	got := queueURLs(t, h.baseURL, accountA, "eu-west-1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "/kept") {
		t.Fatalf("after restore account A has %v, want only kept", got)
	}

	mustStatus(t, http.MethodPost, control+"/reset", `{"services":["sqs"]}`, http.StatusOK)
	if got := queueURLs(t, h.baseURL, accountA, "eu-west-1"); len(got) != 0 {
		t.Fatalf("after an sqs reset account A has %v", got)
	}
}

// Two accounts can each run a cache cluster with the same ID: each gets its
// own engine container, and resetting the service stops both.
func TestMultiAccountEnginesDoNotCollide(t *testing.T) {
	requireDocker(t)
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL, "elasticache", "create-cache-cluster", "--cache-cluster-id", "sessions", "--engine", "redis", "--output", "json")
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "elasticache", "create-cache-cluster", "--cache-cluster-id", "sessions", "--engine", "redis")

	defaultNode := describeCacheCluster(t, h.baseURL, "sessions").Nodes[0].Endpoint
	var scoped struct {
		CacheClusters []cacheCluster `json:"CacheClusters"`
	}
	raw := runAWSAs(t, h.baseURL, accountA, "us-east-1", "elasticache", "describe-cache-clusters", "--cache-cluster-id", "sessions", "--show-cache-node-info")
	if err := json.Unmarshal(raw, &scoped); err != nil || len(scoped.CacheClusters) != 1 || len(scoped.CacheClusters[0].Nodes) != 1 {
		t.Fatalf("describe account A's cluster (%v): %s", err, raw)
	}
	scopedNode := scoped.CacheClusters[0].Nodes[0].Endpoint
	if scopedNode.Port == defaultNode.Port {
		t.Fatalf("both accounts' clusters share port %d", scopedNode.Port)
	}
	assertRedisSpeaks(t, defaultNode.Address, defaultNode.Port)
	assertRedisSpeaks(t, scopedNode.Address, scopedNode.Port)

	mustStatus(t, http.MethodPost, h.baseURL+"/_stratus/control/v1/reset", `{"services":["elasticache"]}`, http.StatusOK)
	assertRefused(t, defaultNode.Address, defaultNode.Port)
	assertRefused(t, scopedNode.Address, scopedNode.Port)
}
