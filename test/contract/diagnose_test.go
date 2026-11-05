package contract

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDiagnosticsExplainRealAndInjectedFailures produces one genuine failure
// and one chaos fault against the real binary, then checks that diagnostics
// tells them apart.
func TestDiagnosticsExplainRealAndInjectedFailures(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	// A genuine bug: reading from a bucket that was never created.
	mustStatus(t, http.MethodGet, h.baseURL+"/never-created/key.txt", "", http.StatusNotFound)

	// An injected fault.
	mustStatus(t, http.MethodPut, h.baseURL+"/diag-bucket", "", http.StatusOK)
	mustStatus(t, http.MethodPut, control+"/chaos", `{"rules":[{"id":"flaky-put","service":"s3","operation":"PutObject","action":"internal_error","max_hits":1}]}`, http.StatusCreated)
	mustStatus(t, http.MethodPut, h.baseURL+"/diag-bucket/a.txt", "x", http.StatusInternalServerError)
	mustStatus(t, http.MethodDelete, control+"/chaos", "", http.StatusOK)

	raw := mustStatus(t, http.MethodGet, control+"/diagnostics", "", http.StatusOK)
	var report struct {
		Summary string `json:"summary"`
		Healthy bool   `json:"healthy"`
		Groups  []struct {
			Code     string `json:"code"`
			Category string `json:"category"`
			YourBug  bool   `json:"your_bug"`
			Count    int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if report.Healthy || len(report.Groups) != 2 {
		t.Fatalf("report: %s", raw)
	}
	real, injected := report.Groups[0], report.Groups[1]
	if real.Category != "missing_resource" || !real.YourBug {
		t.Fatalf("genuine failure misclassified: %+v (%s)", real, raw)
	}
	if injected.Category != "injected_fault" || injected.YourBug {
		t.Fatalf("injected fault misclassified: %+v (%s)", injected, raw)
	}
}
