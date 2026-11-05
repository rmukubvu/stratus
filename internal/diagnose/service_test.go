package diagnose

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type mux struct{ *http.ServeMux }

func (m mux) HandleFunc(p string, fn http.HandlerFunc) { m.ServeMux.HandleFunc(p, fn) }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	svc := NewService(
		func() []Failure {
			return []Failure{
				fail("a", 0, "sqs", "SendMessage", "QueueDoesNotExist", 400),
				fail("b", 10*time.Minute, "s3", "PutObject", "SlowDown", 503),
			}
		},
		func() map[string]Injection { return map[string]Injection{"b": {RuleID: "r", Action: "throttle"}} },
		func() time.Time { return t0.Add(11 * time.Minute) },
	)
	m := mux{http.NewServeMux()}
	RegisterRoutes(m, svc)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestDiagnosticsEndpoint(t *testing.T) {
	srv := newTestServer(t)
	base := srv.URL + "/_stratus/control/v1/diagnostics"

	status, body := get(t, base)
	if status != 200 || body["failures"] != float64(2) || body["healthy"] != false {
		t.Fatalf("all: %d %v", status, body)
	}
	_, body = get(t, base+"?since=5m")
	if body["failures"] != float64(1) || body["healthy"] != true {
		t.Fatalf("since: %v", body)
	}
	_, body = get(t, base+"?exclude_injected=true&service=s3")
	if body["failures"] != float64(0) {
		t.Fatalf("filters: %v", body)
	}
	for _, q := range []string{"?since=soon", "?exclude_injected=maybe", "?max_groups=0"} {
		if status, body := get(t, base+q); status != 400 || body["error"] == nil {
			t.Errorf("%s: %d %v", q, status, body)
		}
	}
}
