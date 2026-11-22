package clock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/control"
)

func newMux(t *testing.T) (*http.ServeMux, *realTime) {
	t.Helper()
	c, _, real := newClock()
	mux := http.NewServeMux()
	RegisterRoutes(muxRouter{mux}, c)
	return mux, real
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, control.PathPrefix+path, strings.NewReader(body)))
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: response is not JSON: %s", method, path, rec.Body.String())
	}
	return rec.Code, out
}

func TestClockRoutesAdvanceFreezeAndResume(t *testing.T) {
	mux, real := newMux(t)

	code, out := do(t, mux, "POST", "/clock/advance", `{"by":"2h"}`)
	if code != 200 || out["now"] != t0.Add(2*time.Hour).Format(time.RFC3339) || out["offset"] != "2h0m0s" {
		t.Fatalf("advance by: %d %v", code, out)
	}
	code, out = do(t, mux, "POST", "/clock/advance", `{"to":"2026-11-23T09:00:00Z"}`)
	if code != 200 || out["now"] != "2026-11-23T09:00:00Z" {
		t.Fatalf("advance to: %d %v", code, out)
	}
	if _, ok := out["effects"].([]any); !ok {
		t.Fatalf("advance result has no effects list: %v", out)
	}

	if code, out = do(t, mux, "POST", "/clock/freeze", ""); code != 200 || out["frozen"] != true {
		t.Fatalf("freeze: %d %v", code, out)
	}
	real.t = real.t.Add(time.Hour)
	if _, out = do(t, mux, "GET", "/clock", ""); out["now"] != "2026-11-23T09:00:00Z" {
		t.Fatalf("frozen clock moved: %v", out)
	}
	if code, out = do(t, mux, "POST", "/clock/resume", ""); code != 200 || out["frozen"] != false {
		t.Fatalf("resume: %d %v", code, out)
	}
}

func TestClockRoutesRefuseBadMovesWithAHint(t *testing.T) {
	mux, _ := newMux(t)
	for _, tc := range []struct{ body, code string }{
		{`{}`, "InvalidRequest"},
		{`{"by":"soon"}`, "InvalidRequest"},
		{`{"to":"tomorrow"}`, "InvalidRequest"},
		{`{"by":"1h","to":"2026-11-23T09:00:00Z"}`, "InvalidRequest"},
		{`{"by":"-5m"}`, "ClockNotForward"},
		{`{"to":"2020-01-01T00:00:00Z"}`, "ClockNotForward"},
	} {
		status, out := do(t, mux, "POST", "/clock/advance", tc.body)
		e, _ := out["error"].(map[string]any)
		if status != http.StatusBadRequest || e["code"] != tc.code || e["hint"] == "" {
			t.Errorf("%s: %d %v, want 400 %s with a hint", tc.body, status, out, tc.code)
		}
	}
}

type muxRouter struct{ mux *http.ServeMux }

func (m muxRouter) HandleFunc(pattern string, fn http.HandlerFunc) { m.mux.HandleFunc(pattern, fn) }
