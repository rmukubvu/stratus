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
}

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
}
