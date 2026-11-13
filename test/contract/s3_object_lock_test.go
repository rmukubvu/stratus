package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAWSCLIS3ObjectLock drives an audit-bucket workflow through the AWS CLI:
// a compliance-locked record survives every delete, a governance-locked one
// yields only to the bypass header, and a legal hold blocks until released.
func TestAWSCLIS3ObjectLock(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL, "s3api", "create-bucket", "--bucket", "audit", "--object-lock-enabled-for-bucket")
	lockConfig := runAWS(t, h.baseURL, "s3api", "get-object-lock-configuration", "--bucket", "audit", "--output", "json")
	if !strings.Contains(string(lockConfig), "Enabled") {
		t.Fatalf("object lock configuration: %s", lockConfig)
	}

	bodyPath := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(bodyPath, []byte(`{"decision":42}`), 0o644); err != nil {
		t.Fatalf("write body: %v", err)
	}
	until := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	putLocked := func(key, mode string) string {
		var out struct{ VersionId string }
		mustDecode(t, runAWS(t, h.baseURL, "s3api", "put-object", "--bucket", "audit", "--key", key, "--body", bodyPath,
			"--object-lock-mode", mode, "--object-lock-retain-until-date", until, "--output", "json"), &out)
		if out.VersionId == "" {
			t.Fatalf("put-object returned no version ID")
		}
		return out.VersionId
	}

	t.Run("compliance", func(t *testing.T) {
		version := putLocked("compliance.json", "COMPLIANCE")
		var retention struct {
			Retention struct{ Mode string }
		}
		mustDecode(t, runAWS(t, h.baseURL, "s3api", "get-object-retention", "--bucket", "audit", "--key", "compliance.json", "--output", "json"), &retention)
		if retention.Retention.Mode != "COMPLIANCE" {
			t.Fatalf("retention mode %q", retention.Retention.Mode)
		}
		out := runAWSExpectingFailure(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "compliance.json",
			"--version-id", version, "--bypass-governance-retention")
		if !strings.Contains(out, "AccessDenied") {
			t.Fatalf("expected AccessDenied deleting a compliance-locked version: %s", out)
		}
		// A delete without a version ID adds a marker and the version stays.
		runAWS(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "compliance.json")
		runAWS(t, h.baseURL, "s3api", "head-object", "--bucket", "audit", "--key", "compliance.json", "--version-id", version)
	})

	t.Run("governance", func(t *testing.T) {
		version := putLocked("governance.json", "GOVERNANCE")
		out := runAWSExpectingFailure(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "governance.json", "--version-id", version)
		if !strings.Contains(out, "AccessDenied") {
			t.Fatalf("expected AccessDenied without bypass: %s", out)
		}
		runAWS(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "governance.json", "--version-id", version, "--bypass-governance-retention")
	})

	t.Run("legal hold", func(t *testing.T) {
		var out struct{ VersionId string }
		mustDecode(t, runAWS(t, h.baseURL, "s3api", "put-object", "--bucket", "audit", "--key", "held.json", "--body", bodyPath,
			"--object-lock-legal-hold-status", "ON", "--output", "json"), &out)
		failure := runAWSExpectingFailure(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "held.json", "--version-id", out.VersionId)
		if !strings.Contains(failure, "AccessDenied") {
			t.Fatalf("expected AccessDenied under legal hold: %s", failure)
		}
		runAWS(t, h.baseURL, "s3api", "put-object-legal-hold", "--bucket", "audit", "--key", "held.json", "--legal-hold", "Status=OFF")
		runAWS(t, h.baseURL, "s3api", "delete-object", "--bucket", "audit", "--key", "held.json", "--version-id", out.VersionId)
	})

	t.Run("if-none-match", func(t *testing.T) {
		runAWS(t, h.baseURL, "s3api", "put-object", "--bucket", "audit", "--key", "checkpoint-1", "--body", bodyPath, "--if-none-match", "*")
		out := runAWSExpectingFailure(t, h.baseURL, "s3api", "put-object", "--bucket", "audit", "--key", "checkpoint-1", "--body", bodyPath, "--if-none-match", "*")
		if !strings.Contains(out, "PreconditionFailed") {
			t.Fatalf("expected PreconditionFailed on a second If-None-Match write: %s", out)
		}
	})

	versions := runAWS(t, h.baseURL, "s3api", "list-object-versions", "--bucket", "audit", "--output", "json")
	if !strings.Contains(string(versions), "compliance.json") || !strings.Contains(string(versions), "DeleteMarkers") {
		t.Fatalf("list-object-versions should show the locked version and its delete marker: %s", versions)
	}
}
