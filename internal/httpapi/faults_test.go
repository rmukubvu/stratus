package httpapi

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/chaos"
)

type stubDecider struct {
	decision chaos.Decision
	seen     []chaos.Request
}

func (s *stubDecider) Decide(req chaos.Request) chaos.Decision {
	s.seen = append(s.seen, req)
	return s.decision
}

func newFaultServer(t *testing.T, d chaos.Decision, coldStarts *[]string) (*httptest.Server, *stubDecider) {
	t.Helper()
	stub := &stubDecider{decision: d}
	srv := httptest.NewServer(NewServer(Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Faults: stub,
		ColdStart: func(_ context.Context, fn string) error {
			if coldStarts != nil {
				*coldStarts = append(*coldStarts, fn)
			}
			return nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv, stub
}

func TestInjectedErrorIsShapedForJSONProtocol(t *testing.T) {
	srv, stub := newFaultServer(t, chaos.Decision{
		RuleIDs: []string{"throttle-sqs"},
		Error:   &chaos.ErrorSpec{StatusCode: 400, Code: "ThrottlingException", Message: "Rate exceeded"},
	}, nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(`{}`))
	req.Header.Set("X-Amz-Target", "AmazonSQS.SendMessage")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 400 || body["__type"] != "ThrottlingException" {
		t.Fatalf("got %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get(FaultHeader) != "throttle-sqs" {
		t.Fatalf("fault header = %q", resp.Header.Get(FaultHeader))
	}
	if got := stub.seen[0]; got.Service != "sqs" || got.Operation != "SendMessage" || got.RequestID == "" {
		t.Fatalf("decider saw %+v", got)
	}
}

func TestInjectedErrorIsShapedForS3(t *testing.T) {
	srv, stub := newFaultServer(t, chaos.Decision{
		RuleIDs: []string{"s3"},
		Error:   &chaos.ErrorSpec{StatusCode: 503, Code: "SlowDown", Message: "slow down"},
	}, nil)

	resp, err := http.Get(srv.URL + "/assets/logo.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 503 || body.Code != "SlowDown" {
		t.Fatalf("got %d %+v", resp.StatusCode, body)
	}
	if stub.seen[0].Bucket != "assets" {
		t.Fatalf("bucket not passed: %+v", stub.seen[0])
	}
}

func TestInjectedDelayIsApplied(t *testing.T) {
	srv, _ := newFaultServer(t, chaos.Decision{
		RuleIDs: []string{"slow"},
		Delay:   120 * time.Millisecond,
		Error:   &chaos.ErrorSpec{StatusCode: 500, Code: "InternalFailure", Message: "x"},
	}, nil)
	start := time.Now()
	resp, err := http.Get(srv.URL + "/assets/logo.png")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("request took %s, expected at least 120ms", elapsed)
	}
}

func TestDropClosesConnectionWithoutResponse(t *testing.T) {
	srv, _ := newFaultServer(t, chaos.Decision{RuleIDs: []string{"drop"}, Drop: true}, nil)
	_, err := http.Get(srv.URL + "/assets/logo.png")
	if err == nil {
		t.Fatal("expected a connection error")
	}
}

func TestColdStartRecyclesInvokedFunction(t *testing.T) {
	var cold []string
	srv, stub := newFaultServer(t, chaos.Decision{
		RuleIDs:   []string{"cold", "fail"},
		ColdStart: "checkout",
		Error:     &chaos.ErrorSpec{StatusCode: 500, Code: "ServiceException", Message: "x"},
	}, &cold)
	resp, err := http.Post(srv.URL+"/2015-03-31/functions/arn:aws:lambda:us-east-1:000000000000:function:checkout:live/invocations", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(cold) != 1 || cold[0] != "checkout" {
		t.Fatalf("cold starts: %v", cold)
	}
	if stub.seen[0].Function != "checkout" || stub.seen[0].Operation != "Invoke" {
		t.Fatalf("decider saw %+v", stub.seen[0])
	}
}

func TestFunctionErrorAnswersLikeAThrowingFunction(t *testing.T) {
	srv, _ := newFaultServer(t, chaos.Decision{RuleIDs: []string{"fail-checkout"}, FunctionError: "boom"}, nil)
	resp, err := http.Post(srv.URL+"/2015-03-31/functions/checkout/invocations", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Function-Error") != "Unhandled" ||
		resp.Header.Get(FaultHeader) != "fail-checkout" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	if body["errorType"] != chaos.FunctionErrorType || body["errorMessage"] != "boom" {
		t.Fatalf("payload: %v", body)
	}
}

func TestHealthEndpointsAreNeverFaulted(t *testing.T) {
	srv, stub := newFaultServer(t, chaos.Decision{RuleIDs: []string{"all"}, Drop: true}, nil)
	resp, err := http.Get(srv.URL + "/_stratus/health")
	if err != nil {
		t.Fatalf("health faulted: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(stub.seen) != 0 {
		t.Fatalf("status=%d decider calls=%d", resp.StatusCode, len(stub.seen))
	}
}

func TestLambdaFunctionName(t *testing.T) {
	cases := map[string]string{
		"/2015-03-31/functions/checkout/invocations":                                                "checkout",
		"/2015-03-31/functions/arn:aws:lambda:us-east-1:000000000000:function:checkout/invocations": "checkout",
		"/2015-03-31/functions/arn:aws:lambda:us-east-1:000000000000:function:checkout:3":           "checkout",
		"/2015-03-31/event-source-mappings":                                                         "",
	}
	for path, want := range cases {
		if got := lambdaFunctionName("lambda", path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
	if got := lambdaFunctionName("s3", "/2015-03-31/functions/x"); got != "" {
		t.Errorf("non-lambda service: got %q", got)
	}
}
