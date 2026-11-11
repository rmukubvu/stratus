package contract

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// tryAWSWith runs the AWS CLI with the given credentials and returns its
// output and whether it succeeded.
func tryAWSWith(t *testing.T, endpoint string, creds assumedCredentials, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("aws", append([]string{"--endpoint-url", endpoint, "--region", "us-east-1", "--output", "json"}, args...)...)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+creds.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.Credentials.SecretAccessKey,
		"AWS_SESSION_TOKEN="+creds.Credentials.SessionToken,
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
		"AWS_MAX_ATTEMPTS=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func createUserKey(t *testing.T, endpoint, account, user string) assumedCredentials {
	t.Helper()
	runAWSAs(t, endpoint, account, "us-east-1", "iam", "create-user", "--user-name", user)
	var created struct {
		AccessKey struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
		} `json:"AccessKey"`
	}
	raw := runAWSAs(t, endpoint, account, "us-east-1", "iam", "create-access-key", "--user-name", user)
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create-access-key: %v\n%s", err, raw)
	}
	var creds assumedCredentials
	creds.Credentials.AccessKeyID = created.AccessKey.AccessKeyID
	creds.Credentials.SecretAccessKey = created.AccessKey.SecretAccessKey
	return creds
}

func TestIAMEnforceRefusesWhatPoliciesDoNotAllow(t *testing.T) {
	h := startHarnessWithEnv(t, t.TempDir(), "STRATUS_IAM=enforce")
	defer h.Close()

	// The account root (a twelve-digit key) is never refused.
	dev := createUserKey(t, h.baseURL, accountA, "dev")
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "put-user-policy", "--user-name", "dev", "--policy-name", "app-queues",
		"--policy-document", `{"Version":"2012-10-17","Statement":[
			{"Effect":"Allow","Action":"sqs:*","Resource":"arn:aws:sqs:*:`+accountA+`:app-*"},
			{"Effect":"Allow","Action":"sqs:ListQueues","Resource":"*"},
			{"Effect":"Deny","Action":"sqs:DeleteQueue","Resource":"*"}]}`)

	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "create-queue", "--queue-name", "app-orders"); !ok {
		t.Fatalf("an allowed create-queue failed: %s", out)
	}
	out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "create-queue", "--queue-name", "billing")
	if ok || !strings.Contains(out, "AccessDenied") || !strings.Contains(out, "user/dev") {
		t.Fatalf("create-queue outside the policy should be refused, got ok=%v: %s", ok, out)
	}
	out, ok = tryAWSWith(t, h.baseURL, dev, "sqs", "delete-queue", "--queue-url", h.baseURL+"/"+accountA+"/app-orders")
	if ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("an explicitly denied delete-queue should be refused, got ok=%v: %s", ok, out)
	}
	if out, ok := tryAWSWith(t, h.baseURL, dev, "s3api", "list-buckets"); ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("s3 without a policy should be refused, got ok=%v: %s", ok, out)
	}
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sts", "get-caller-identity"); !ok {
		t.Fatalf("get-caller-identity needs no permission: %s", out)
	}

	// A role session gets the role's policies.
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-role", "--role-name", "reader",
		"--assume-role-policy-document", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"`+accountA+`"},"Action":"sts:AssumeRole"}]}`)
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "attach-role-policy", "--role-name", "reader", "--policy-arn", "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess")
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "s3api", "create-bucket", "--bucket", "reports")
	reader := assumeRole(t, h.baseURL, accountA, "arn:aws:iam::"+accountA+":role/reader", "audit")
	if out, ok := tryAWSWith(t, h.baseURL, reader, "s3api", "list-objects-v2", "--bucket", "reports"); !ok {
		t.Fatalf("the reader role should list objects: %s", out)
	}
	if out, ok := tryAWSWith(t, h.baseURL, reader, "s3api", "create-bucket", "--bucket", "not-allowed"); ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("the reader role should not create buckets, got ok=%v: %s", ok, out)
	}

	// A deactivated key stops working.
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "update-access-key", "--user-name", "dev",
		"--access-key-id", dev.Credentials.AccessKeyID, "--status", "Inactive")
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "list-queues"); ok || !strings.Contains(out, "InvalidClientTokenId") {
		t.Fatalf("an inactive key should be refused, got ok=%v: %s", ok, out)
	}
}

func TestIAMOffByDefault(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	dev := createUserKey(t, h.baseURL, accountA, "dev")
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "create-queue", "--queue-name", "anything"); !ok {
		t.Fatalf("without STRATUS_IAM, a user with no policies can still do everything: %s", out)
	}
}
