package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordingObserver struct {
	mu   sync.Mutex
	seen []RequestOutcome
	deaf bool
}

func (o *recordingObserver) Listening() bool { return !o.deaf }

func (o *recordingObserver) Observe(r RequestOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, r)
}

func TestObserverSeesAWSRequestsWithTheirLimitRule(t *testing.T) {
	obs := &recordingObserver{}
	srv := httptest.NewServer(NewServer(Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits:   &stubGate{},
		Observer: obs,
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(`{"Name":"p"}`))
	req.Header.Set("X-Amz-Target", "AmazonSSM.GetParameter")
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("amz-sdk-invocation-id", "inv-1")
	req.Header.Set("amz-sdk-request", "attempt=2; max=3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Control API calls are not AWS traffic.
	if resp, err := http.Get(srv.URL + "/_stratus/control/v1/health"); err == nil {
		resp.Body.Close()
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.seen) != 1 {
		t.Fatalf("observed %d requests, want 1: %+v", len(obs.seen), obs.seen)
	}
	got := obs.seen[0]
	if got.Service != "ssm" || got.Operation != "GetParameter" || got.Status != http.StatusBadRequest ||
		got.Code != "ThrottlingException" || got.LimitRule != "rule-1" || got.Time.IsZero() {
		t.Fatalf("outcome: %+v", got)
	}
	if got.InvocationID != "inv-1" || got.Attempt != 2 || got.MaxAttempts != 3 {
		t.Fatalf("sdk retry headers: %+v", got)
	}
}

func TestObserverThatIsNotListeningSeesNothing(t *testing.T) {
	obs := &recordingObserver{deaf: true}
	srv := httptest.NewServer(NewServer(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Observer: obs}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(`{}`))
	req.Header.Set("X-Amz-Target", "AmazonSSM.GetParameter")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if len(obs.seen) != 0 {
		t.Fatalf("observed %d requests while not listening", len(obs.seen))
	}
}

func TestParseSDKRequest(t *testing.T) {
	cases := map[string][2]int{
		"attempt=1; max=3":                       {1, 3},
		"attempt=4;max=10":                       {4, 10},
		"ttl=20250101T000000Z; attempt=2; max=5": {2, 5},
		"":                                       {0, 0},
		"attempt=x; max=3":                       {0, 3},
	}
	for in, want := range cases {
		if a, m := parseSDKRequest(in); a != want[0] || m != want[1] {
			t.Errorf("%q: got %d/%d, want %v", in, a, m, want)
		}
	}
}
