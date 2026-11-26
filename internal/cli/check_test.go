package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFailsWhenTheCodebaseCallsAnOperationStratusLacks(t *testing.T) {
	root := t.TempDir()
	src := "import boto3\nsqs = boto3.client('sqs')\nsqs.send_message()\nsqs.add_permission()\n"
	if err := os.WriteFile(filepath.Join(root, "app.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := RunCheck([]string{root}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "`sqs AddPermission` at app.py:4") {
		t.Fatalf("output:\n%s", out.String())
	}

	out.Reset()
	if code := RunCheck([]string{"--json", root}, &out, &errOut); code != 1 {
		t.Fatalf("--json exit %d", code)
	}
	var report struct {
		Findings []struct{ Operation, Support string } `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Findings) != 2 {
		t.Fatalf("json %s: %v", out.String(), err)
	}
}

func TestCheckPassesWhenEveryCallIsImplemented(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.py"), []byte("import boto3\nboto3.client('sqs').send_message()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := RunCheck([]string{root}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}
