package contract

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestChaosExperimentAgainstRealBinary throttles one S3 bucket, slows
// another, and checks the report attributes every fault to its rule.
func TestChaosExperimentAgainstRealBinary(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	mustStatus(t, http.MethodPut, h.baseURL+"/chaos-flaky", "", http.StatusOK)
	mustStatus(t, http.MethodPut, h.baseURL+"/chaos-slow", "", http.StatusOK)

	mustStatus(t, http.MethodPut, control+"/chaos", `{
		"seed": 1234,
		"rules": [
			{"id": "throttle-flaky", "service": "s3", "operation": "PutObject", "resource": "chaos-flaky", "action": "throttle", "max_hits": 2},
			{"id": "slow-bucket", "service": "s3", "resource": "chaos-slow", "action": "latency", "latency": {"delay_ms": 300}}
		]
	}`, http.StatusCreated)

	// The first two writes to the flaky bucket are throttled, the third succeeds.
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPut, h.baseURL+"/chaos-flaky/a.txt", strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Stratus-Fault") != "throttle-flaky" {
			t.Fatalf("write %d: status %d, fault header %q", i+1, resp.StatusCode, resp.Header.Get("X-Stratus-Fault"))
		}
	}
	mustStatus(t, http.MethodPut, h.baseURL+"/chaos-flaky/a.txt", "x", http.StatusOK)

	start := time.Now()
	mustStatus(t, http.MethodPut, h.baseURL+"/chaos-slow/b.txt", "y", http.StatusOK)
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("slow bucket answered in %s, expected >= 300ms", elapsed)
	}

	// Health checks are never faulted, even during an experiment.
	mustStatus(t, http.MethodGet, control+"/ready", "", http.StatusOK)

	raw := mustStatus(t, http.MethodDelete, control+"/chaos", "", http.StatusOK)
	var report struct {
		Status        string `json:"status"`
		TotalInjected int    `json:"total_injected"`
		Rules         []struct {
			Rule     struct{ ID string } `json:"rule"`
			Matched  int                 `json:"matched"`
			Injected int                 `json:"injected"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decode report: %v (%s)", err, raw)
	}
	if report.Status != "stopped" || report.TotalInjected != 3 {
		t.Fatalf("report: %s", raw)
	}
	if r := report.Rules[0]; r.Matched != 3 || r.Injected != 2 {
		t.Fatalf("throttle rule stats: %+v", r)
	}
}
