package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A function runs with its execution role's session credentials, as in
// Lambda, so its own AWS calls act in its account as its role.
func TestLambdaRunsWithItsExecutionRole(t *testing.T) {
	requireDocker(t)
	h := startHarness(t)
	defer h.Close()

	zipPath := filepath.Join(t.TempDir(), "function.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "import os, boto3\n" +
			"def main(event, context):\n" +
			"    sts = boto3.client('sts', endpoint_url=os.environ['STRATUS_ENDPOINT'])\n" +
			"    return sts.get_caller_identity()\n",
	}); err != nil {
		t.Fatal(err)
	}
	runAWSAs(t, h.baseURL, accountA, "eu-west-1", "lambda", "create-function",
		"--function-name", "whoami",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::"+accountA+":role/service-role/whoami-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
		"--environment", "Variables={STRATUS_ENDPOINT="+dockerReachableEndpoint(h.baseURL)+"}",
	)
	out := filepath.Join(t.TempDir(), "out.json")
	runAWSAs(t, h.baseURL, accountA, "eu-west-1", "lambda", "invoke", "--function-name", "whoami", out)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var id struct{ Account, Arn string }
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatalf("decode function result: %v\n%s", err, raw)
	}
	if id.Account != accountA || id.Arn != "arn:aws:sts::"+accountA+":assumed-role/whoami-role/whoami" {
		t.Fatalf("the function ran as %+v", id)
	}
}
