package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIAMEnforceTrustAndResourcePolicies(t *testing.T) {
	h := startHarnessWithEnv(t, t.TempDir(), "STRATUS_IAM=enforce")
	defer h.Close()
	root := func(args ...string) string { return string(runAWSAs(t, h.baseURL, accountA, "us-east-1", args...)) }

	dev := createUserKey(t, h.baseURL, accountA, "dev")
	other := createUserKey(t, h.baseURL, accountA, "other")
	assumeAny := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sts:AssumeRole","sqs:*"],"Resource":"*"}]}`
	root("iam", "put-user-policy", "--user-name", "dev", "--policy-name", "base", "--policy-document", assumeAny)
	root("iam", "put-user-policy", "--user-name", "other", "--policy-name", "base", "--policy-document", assumeAny)

	// Trust policies decide who may assume a role.
	root("iam", "create-role", "--role-name", "deployer", "--assume-role-policy-document",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::`+accountA+`:user/dev"},"Action":"sts:AssumeRole"}]}`)
	roleARN := "arn:aws:iam::" + accountA + ":role/deployer"
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sts", "assume-role", "--role-arn", roleARN, "--role-session-name", "s1"); !ok {
		t.Fatalf("dev is trusted: %s", out)
	}
	if out, ok := tryAWSWith(t, h.baseURL, other, "sts", "assume-role", "--role-arn", roleARN, "--role-session-name", "s1"); ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("other is not trusted, got ok=%v: %s", ok, out)
	}
	var rootCreds assumedCredentials
	rootCreds.Credentials.AccessKeyID, rootCreds.Credentials.SecretAccessKey = accountA, "test"
	if out, ok := tryAWSWith(t, h.baseURL, rootCreds, "sts", "assume-role", "--role-arn", "arn:aws:iam::"+accountA+":role/missing", "--role-session-name", "s1"); ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("a missing role cannot be assumed, got ok=%v: %s", ok, out)
	}

	// A bucket policy grants access the user's own policies do not.
	root("s3api", "create-bucket", "--bucket", "shared")
	file := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(file, []byte("q3"), 0o644); err != nil {
		t.Fatal(err)
	}
	root("s3api", "put-object", "--bucket", "shared", "--key", "report.txt", "--body", file)
	if out, ok := tryAWSWith(t, h.baseURL, dev, "s3api", "head-object", "--bucket", "shared", "--key", "report.txt"); ok {
		t.Fatalf("dev has no S3 permission yet: %s", out)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + accountA + `:user/dev"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"}]}`
	root("s3api", "put-bucket-policy", "--bucket", "shared", "--policy", policy)
	if got := root("s3api", "get-bucket-policy", "--bucket", "shared"); !strings.Contains(got, "user/dev") {
		t.Fatalf("get-bucket-policy: %s", got)
	}
	if out, ok := tryAWSWith(t, h.baseURL, dev, "s3api", "head-object", "--bucket", "shared", "--key", "report.txt"); !ok {
		t.Fatalf("the bucket policy grants dev reads: %s", out)
	}

	// A queue policy can deny what an identity policy allows.
	url := strings.Trim(root("sqs", "create-queue", "--queue-name", "locked", "--query", "QueueUrl"), "\"\n ")
	root("sqs", "set-queue-attributes", "--queue-url", url, "--attributes",
		`{"Policy":"{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Deny\",\"Principal\":\"*\",\"Action\":\"sqs:SendMessage\",\"Resource\":\"*\"}]}"}`)
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "send-message", "--queue-url", url, "--message-body", "x"); ok || !strings.Contains(out, "AccessDenied") {
		t.Fatalf("the queue policy denies sends, got ok=%v: %s", ok, out)
	}
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "get-queue-attributes", "--queue-url", url); !ok {
		t.Fatalf("other queue actions are still allowed: %s", out)
	}
}
