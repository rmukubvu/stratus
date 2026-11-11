package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

// An IAM user's access key works with the AWS CLI, acts in the user's
// account, and is reported as that user.
func TestIAMUserAccessKeyActsAsTheUser(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-user", "--user-name", "pipeline")
	var created struct {
		AccessKey struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
		} `json:"AccessKey"`
	}
	raw := runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-access-key", "--user-name", "pipeline")
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create-access-key: %v\n%s", err, raw)
	}
	var creds assumedCredentials
	creds.Credentials.AccessKeyID = created.AccessKey.AccessKeyID
	creds.Credentials.SecretAccessKey = created.AccessKey.SecretAccessKey

	var id struct{ Account, Arn string }
	if err := json.Unmarshal(runAWSWith(t, h.baseURL, creds, "eu-west-1", "sts", "get-caller-identity"), &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != accountA || id.Arn != "arn:aws:iam::"+accountA+":user/pipeline" {
		t.Fatalf("caller identity = %+v", id)
	}
	runAWSWith(t, h.baseURL, creds, "eu-west-1", "sqs", "create-queue", "--queue-name", "from-user-key")
	if got := queueURLs(t, h.baseURL, accountA, "eu-west-1"); len(got) != 1 {
		t.Fatalf("account A's queues: %v", got)
	}

	// Policies attach and list the way the AWS CLI, Terraform and CDK expect.
	policy := string(runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-policy",
		"--policy-name", "queues", "--policy-document", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"*"}]}`))
	policyARN := "arn:aws:iam::" + accountA + ":policy/queues"
	if !strings.Contains(policy, policyARN) {
		t.Fatalf("create-policy: %s", policy)
	}
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "attach-user-policy", "--user-name", "pipeline", "--policy-arn", policyARN)
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "attach-user-policy", "--user-name", "pipeline", "--policy-arn", "arn:aws:iam::aws:policy/ReadOnlyAccess")
	attached := string(runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "list-attached-user-policies", "--user-name", "pipeline"))
	if !strings.Contains(attached, policyARN) || !strings.Contains(attached, "ReadOnlyAccess") {
		t.Fatalf("list-attached-user-policies: %s", attached)
	}

	// Keys survive a restart.
	h.Close()
	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()
	if err := json.Unmarshal(runAWSWith(t, h.baseURL, creds, "us-east-1", "sts", "get-caller-identity"), &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != accountA {
		t.Fatalf("after restart the key acts in %q", id.Account)
	}
}
