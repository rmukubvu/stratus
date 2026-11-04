package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestControlSnapshotRestoreAndReset drives the control API against a real
// Stratus binary, using S3 as observable state.
func TestControlSnapshotRestoreAndReset(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	control := h.baseURL + "/_stratus/control/v1"
	mustStatus(t, http.MethodGet, control+"/ready", "", http.StatusOK)

	mustStatus(t, http.MethodPut, h.baseURL+"/control-demo", "", http.StatusOK)
	mustStatus(t, http.MethodPut, h.baseURL+"/control-demo/hello.txt", "v1", http.StatusOK)

	mustStatus(t, http.MethodPost, control+"/snapshots", `{"name":"baseline"}`, http.StatusCreated)

	mustStatus(t, http.MethodPut, h.baseURL+"/control-demo/hello.txt", "v2", http.StatusOK)
	mustStatus(t, http.MethodPut, h.baseURL+"/control-extra", "", http.StatusOK)

	mustStatus(t, http.MethodPost, control+"/snapshots/baseline/restore", "", http.StatusOK)
	if got := body(t, h.baseURL+"/control-demo/hello.txt"); got != "v1" {
		t.Fatalf("object after restore: got %q, want v1", got)
	}
	if listing := body(t, h.baseURL+"/"); strings.Contains(listing, "control-extra") {
		t.Fatalf("bucket created after snapshot survived restore: %s", listing)
	}

	resp := mustStatus(t, http.MethodPost, control+"/reset", `{"services":["s3"]}`, http.StatusOK)
	var reset struct {
		Services []string `json:"services"`
	}
	if err := json.Unmarshal([]byte(resp), &reset); err != nil || len(reset.Services) != 1 || reset.Services[0] != "s3" {
		t.Fatalf("reset response: %s (%v)", resp, err)
	}
	mustStatus(t, http.MethodGet, h.baseURL+"/control-demo/hello.txt", "", http.StatusNotFound)

	// The snapshot is still there after reset and can be restored again.
	mustStatus(t, http.MethodPost, control+"/snapshots/baseline/restore", "", http.StatusOK)
	if got := body(t, h.baseURL+"/control-demo/hello.txt"); got != "v1" {
		t.Fatalf("object after second restore: got %q, want v1", got)
	}
}

func mustStatus(t *testing.T, method, url, payload string, want int) string {
	t.Helper()
	var reader io.Reader
	if payload != "" {
		reader = strings.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, url, resp.StatusCode, want, raw)
	}
	return string(raw)
}

func body(t *testing.T, url string) string {
	t.Helper()
	return strings.TrimSpace(mustStatus(t, http.MethodGet, url, "", http.StatusOK))
}
