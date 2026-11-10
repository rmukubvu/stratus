package contract

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type assumedCredentials struct {
	Credentials struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
	} `json:"Credentials"`
	AssumedRoleUser struct {
		Arn string `json:"Arn"`
	} `json:"AssumedRoleUser"`
}

// runAWSWith runs the AWS CLI signed with session credentials.
func runAWSWith(t *testing.T, endpoint string, creds assumedCredentials, region string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("aws", append([]string{"--endpoint-url", endpoint, "--region", region, "--output", "json"}, args...)...)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+creds.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.Credentials.SecretAccessKey,
		"AWS_SESSION_TOKEN="+creds.Credentials.SessionToken,
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws %s with session credentials failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func assumeRole(t *testing.T, endpoint, fromAccount, roleARN, session string) assumedCredentials {
	t.Helper()
	var creds assumedCredentials
	raw := runAWSAs(t, endpoint, fromAccount, "us-east-1", "sts", "assume-role", "--role-arn", roleARN, "--role-session-name", session)
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("decode assume-role: %v\n%s", err, raw)
	}
	return creds
}

func TestAssumeRoleActsInTheRoleAccount(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	roleARN := "arn:aws:iam::" + accountB + ":role/deployer"
	creds := assumeRole(t, h.baseURL, accountA, roleARN, "pipeline")
	if !strings.HasPrefix(creds.Credentials.AccessKeyID, "ASIA") {
		t.Fatalf("session access key = %q", creds.Credentials.AccessKeyID)
	}
	if want := "arn:aws:sts::" + accountB + ":assumed-role/deployer/pipeline"; creds.AssumedRoleUser.Arn != want {
		t.Fatalf("assumed role ARN = %q, want %q", creds.AssumedRoleUser.Arn, want)
	}

	var id struct{ Account, Arn string }
	if err := json.Unmarshal(runAWSWith(t, h.baseURL, creds, "eu-west-1", "sts", "get-caller-identity"), &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != accountB || id.Arn != creds.AssumedRoleUser.Arn {
		t.Fatalf("caller identity with session credentials = %+v", id)
	}

	// Resources created with the session belong to the role's account.
	runAWSWith(t, h.baseURL, creds, "eu-west-1", "sqs", "create-queue", "--queue-name", "deployments")
	if got := queueURLs(t, h.baseURL, accountB, "eu-west-1"); len(got) != 1 || !strings.Contains(got[0], "/"+accountB+"/deployments") {
		t.Fatalf("account B's queues: %v", got)
	}
	if got := queueURLs(t, h.baseURL, accountA, "eu-west-1"); len(got) != 0 {
		t.Fatalf("the caller's own account should be untouched: %v", got)
	}

	// Sessions survive a restart.
	h.Close()
	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()
	if err := json.Unmarshal(runAWSWith(t, h.baseURL, creds, "us-east-1", "sts", "get-caller-identity"), &id); err != nil {
		t.Fatal(err)
	}
	if id.Account != accountB {
		t.Fatalf("after restart the session acts in %q", id.Account)
	}
}
