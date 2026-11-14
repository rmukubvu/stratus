package httpapi

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/limits"
)

type stubGate struct{ seen []limits.Request }

func (g *stubGate) Admit(req limits.Request) *limits.Breach {
	g.seen = append(g.seen, req)
	return &limits.Breach{RuleID: "rule-1", Kind: limits.KindRate, Service: req.Service, Operation: req.Operation}
}

func newLimitServer(t *testing.T) (*httptest.Server, *stubGate) {
	t.Helper()
	gate := &stubGate{}
	srv := httptest.NewServer(NewServer(Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits: gate,
	}))
	t.Cleanup(srv.Close)
	return srv, gate
}

func TestLimitBreachReturnsJSONThrottle(t *testing.T) {
	srv, gate := newLimitServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(`{"Name":"p"}`))
	req.Header.Set("X-Amz-Target", "AmazonSSM.GetParameter")
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusBadRequest || body["__type"] != "ThrottlingException" || resp.Header.Get(LimitHeader) != "rule-1" {
		t.Fatalf("got %d %v header %q", resp.StatusCode, body, resp.Header.Get(LimitHeader))
	}
	if len(gate.seen) != 1 || gate.seen[0].Service != "ssm" || gate.seen[0].Operation != "GetParameter" || gate.seen[0].Account == "" {
		t.Fatalf("gate saw %+v", gate.seen)
	}
}

func TestLimitBreachReturnsS3SlowDownWithPrefix(t *testing.T) {
	srv, gate := newLimitServer(t)
	resp, err := http.Get(srv.URL + "/photos/2026/11/cat.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusServiceUnavailable || body.Code != "SlowDown" {
		t.Fatalf("got %d %q", resp.StatusCode, body.Code)
	}
	if got := gate.seen[0]; got.Resource != "photos" || got.Prefix != "photos/2026/11/" || got.Operation != "GetObject" {
		t.Fatalf("gate saw %+v", got)
	}
}

func TestLimitsSkipStratusEndpoints(t *testing.T) {
	srv, gate := newLimitServer(t)
	resp, err := http.Get(srv.URL + "/_stratus/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(gate.seen) != 0 {
		t.Fatalf("health check went through the limits gate: %+v", gate.seen)
	}
}

func TestS3Prefix(t *testing.T) {
	cases := map[[2]string]string{
		{"b", "k"}:         "b/",
		{"b", "a/b/c.txt"}: "b/a/b/",
		{"b", ""}:          "b/",
	}
	for in, want := range cases {
		if got := s3Prefix(in[0], in[1]); got != want {
			t.Errorf("s3Prefix(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
