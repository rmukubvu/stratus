package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStrictSignatureMode runs the real AWS CLI against Stratus with
// STRATUS_SIGV4=strict: correctly signed requests from every kind of
// credential work, and signing mistakes fail with the codes AWS returns.
func TestStrictSignatureMode(t *testing.T) {
	// CLI v1 presigns S3 URLs with SigV2 unless told otherwise; v2 already
	// uses SigV4.
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("[default]\ns3 =\n    signature_version = s3v4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", config)

	h := startHarnessWithEnv(t, t.TempDir(), "STRATUS_SIGV4=strict")
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	// Root credentials ("test" or an account ID, secret "test") sign fine.
	runAWS(t, h.baseURL, "sts", "get-caller-identity")
	runAWS(t, h.baseURL, "s3", "mb", "s3://signed")
	file := filepath.Join(t.TempDir(), "report name+1.txt")
	if err := os.WriteFile(file, []byte("quarterly numbers"), 0o600); err != nil {
		t.Fatal(err)
	}
	runAWS(t, h.baseURL, "s3", "cp", file, "s3://signed/reports/report name+1.txt")
	runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "orders")
	runAWS(t, h.baseURL, "dynamodb", "list-tables")
	runAWSAs(t, h.baseURL, accountA, "eu-west-1", "sqs", "list-queues")

	// IAM user keys and STS session credentials sign with their own secrets.
	dev := createUserKey(t, h.baseURL, accountA, "dev")
	if out, ok := tryAWSWith(t, h.baseURL, dev, "sqs", "list-queues"); !ok {
		t.Fatalf("an IAM user key should verify: %s", out)
	}
	runAWSAs(t, h.baseURL, accountA, "us-east-1", "iam", "create-role", "--role-name", "app",
		"--assume-role-policy-document", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"`+accountA+`"},"Action":"sts:AssumeRole"}]}`)
	session := assumeRole(t, h.baseURL, accountA, "arn:aws:iam::"+accountA+":role/app", "strict")
	if out, ok := tryAWSWith(t, h.baseURL, session, "dynamodb", "list-tables"); !ok {
		t.Fatalf("session credentials should verify: %s", out)
	}

	// A presigned URL works until it is tampered with.
	url := strings.TrimSpace(string(runAWS(t, h.baseURL, "s3", "presign", "s3://signed/reports/report name+1.txt", "--expires-in", "60")))
	if status, body := get(t, url); status != http.StatusOK || body != "quarterly numbers" {
		t.Fatalf("presigned GET: %d %s", status, body)
	}
	if status, body := get(t, strings.Replace(url, "report%20name", "other%20name", 1)); status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("a tampered presigned URL should be refused: %d %s", status, body)
	}

	// Signing mistakes fail with each service's own error code.
	wrongSecret := assumedCredentials{}
	wrongSecret.Credentials.AccessKeyID = "test"
	wrongSecret.Credentials.SecretAccessKey = "not-the-secret"
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"sts", "get-caller-identity"}, "SignatureDoesNotMatch"},
		{[]string{"dynamodb", "list-tables"}, "InvalidSignatureException"},
		{[]string{"s3api", "list-objects-v2", "--bucket", "signed"}, "SignatureDoesNotMatch"},
	} {
		if out, ok := tryAWSWith(t, h.baseURL, wrongSecret, tc.args...); ok || !strings.Contains(out, tc.code) {
			t.Fatalf("%v with the wrong secret: want %s, got ok=%v: %s", tc.args, tc.code, ok, out)
		}
	}
	unknown := assumedCredentials{}
	unknown.Credentials.AccessKeyID = "AKIAUNKNOWNKEY000000"
	unknown.Credentials.SecretAccessKey = "x"
	if out, ok := tryAWSWith(t, h.baseURL, unknown, "sqs", "list-queues"); ok || !(strings.Contains(out, "InvalidClientTokenId") || strings.Contains(out, "UnrecognizedClient")) {
		t.Fatalf("an unknown key should be refused: ok=%v %s", ok, out)
	}
	noToken := session
	noToken.Credentials.SessionToken = ""
	if out, ok := tryAWSWith(t, h.baseURL, noToken, "s3api", "list-buckets"); ok || !strings.Contains(out, "InvalidToken") {
		t.Fatalf("session credentials without their token should be refused: ok=%v %s", ok, out)
	}
	if status, body := get(t, h.baseURL+"/signed/reports/report%20name+1.txt"); status != http.StatusForbidden || !strings.Contains(body, "AccessDenied") {
		t.Fatalf("an anonymous GET should be refused: %d %s", status, body)
	}

	// The control API shows what Stratus expected, and diagnostics blames
	// the signer.
	var status struct {
		Mode   string `json:"mode"`
		Failed int    `json:"failed"`
		Recent []struct {
			Code             string `json:"code"`
			CanonicalRequest string `json:"canonical_request"`
		} `json:"recent_failures"`
	}
	if err := json.Unmarshal([]byte(mustStatus(t, http.MethodGet, control+"/sigv4", "", http.StatusOK)), &status); err != nil {
		t.Fatal(err)
	}
	if status.Mode != "strict" || status.Failed < 6 || len(status.Recent) == 0 {
		t.Fatalf("sigv4 status: %+v", status)
	}
	raw := mustStatus(t, http.MethodGet, control+"/diagnostics?service=dynamodb", "", http.StatusOK)
	var report struct {
		Groups []struct {
			Category string `json:"category"`
			YourBug  bool   `json:"your_bug"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 1 || report.Groups[0].Category != "signature" || !report.Groups[0].YourBug {
		t.Fatalf("diagnostics should report the signing bug: %s", raw)
	}

	// Strict mode can be turned off at runtime.
	mustStatus(t, http.MethodPut, control+"/sigv4", `{"mode":"off"}`, http.StatusOK)
	if out, ok := tryAWSWith(t, h.baseURL, wrongSecret, "sts", "get-caller-identity"); !ok {
		t.Fatalf("with signature mode off any secret works: %s", out)
	}
}

// TestSignatureModeOffByDefault keeps the default permissive.
func TestSignatureModeOffByDefault(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	cmd := exec.Command("aws", "--endpoint-url", h.baseURL, "--region", "us-east-1", "sts", "get-caller-identity")
	cmd.Env = append(os.Environ(), "AWS_ACCESS_KEY_ID=anything", "AWS_SECRET_ACCESS_KEY=whatever", "AWS_EC2_METADATA_DISABLED=true", "AWS_PAGER=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("by default Stratus accepts any credentials: %v\n%s", err, out)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}
