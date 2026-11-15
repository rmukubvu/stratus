package app

import (
	"context"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/diagnose"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

var esm = chaos.Delivery{
	Source: "sqs", SourceARN: "arn:aws:sqs:us-east-1:000000000000:orders",
	Service: "lambda", Operation: "Invoke", TargetARN: "arn:aws:lambda:us-east-1:000000000000:function:process-order",
	Function: "process-order", Async: true,
}

func testGate(next chaos.Gate) (recordingGate, *diagnose.DeliveryLog) {
	log := diagnose.NewDeliveryLog(0)
	g := newRecordingGate(next, log)
	g.now = func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) }
	g.newID = func() string { return "delivery-1" }
	return g, log
}

func TestRecordingGateLogsAFunctionThatThrew(t *testing.T) {
	g, log := testGate(chaos.Passthrough{})
	err := g.Deliver(context.Background(), esm, func(context.Context) error {
		return &lambdasvc.FunctionError{Function: "process-order", Kind: "Unhandled", Type: "TypeError", Message: "Float types are not supported"}
	})
	if err == nil {
		t.Fatal("the delivery's error must still reach the source")
	}
	got := log.Failures()
	if len(got) != 1 {
		t.Fatalf("failures: %+v", got)
	}
	f := got[0]
	if f.Code != diagnose.FunctionErrorCode || f.Message != "TypeError: Float types are not supported" || f.RequestID != "delivery-1" ||
		f.Service != "lambda" || f.Delivery == nil || f.Delivery.Source != "sqs" || f.Path != esm.TargetARN {
		t.Fatalf("failure: %+v", f)
	}
}

func TestRecordingGateKeepsAnAPIErrorsCode(t *testing.T) {
	g, log := testGate(chaos.Passthrough{})
	_ = g.Deliver(context.Background(), esm, func(context.Context) error {
		return &apierror.Error{StatusCode: 503, Code: "ServiceException", Message: "lambda image is not available locally"}
	})
	if f := log.Failures()[0]; f.Code != "ServiceException" || f.Status != 503 {
		t.Fatalf("failure: %+v", f)
	}
}

func TestRecordingGateIgnoresSuccessAndCancellation(t *testing.T) {
	g, log := testGate(chaos.Passthrough{})
	_ = g.Deliver(context.Background(), esm, func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = g.Deliver(ctx, esm, func(context.Context) error { return context.Canceled })
	if got := log.Failures(); len(got) != 0 {
		t.Fatalf("failures: %+v", got)
	}
}

// idGate records the delivery it was handed, as the chaos injector would
// see it.
type idGate struct{ seen chaos.Delivery }

func (g *idGate) Deliver(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
	g.seen = d
	return deliver(ctx)
}

func TestRecordingGateNamesDeliveriesSoChaosHitsCanBeMatched(t *testing.T) {
	next := &idGate{}
	g, _ := testGate(next)
	_ = g.Deliver(context.Background(), esm, func(context.Context) error { return nil })
	if next.seen.ID != "delivery-1" {
		t.Fatalf("delivery ID %q", next.seen.ID)
	}
	withID := esm
	withID.ID = "message-7"
	_ = g.Deliver(context.Background(), withID, func(context.Context) error { return nil })
	if next.seen.ID != "message-7" {
		t.Fatalf("a source's own ID must be kept, got %q", next.seen.ID)
	}
}

func TestInjectedDeliveryFailuresKeepOnlyFaultsThatStopADelivery(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	hits := []chaos.Hit{
		{Time: at, RuleID: "boom", Action: chaos.ActionFunctionError, Scope: chaos.ScopeEvent, RequestID: "d1", Service: "lambda", Operation: "Invoke", Source: "sqs", Path: esm.TargetARN},
		{Time: at, RuleID: "slow", Action: chaos.ActionLatency, Scope: chaos.ScopeEvent, RequestID: "d2", Service: "lambda"},
		{Time: at, RuleID: "api", Action: chaos.ActionThrottle, Scope: chaos.ScopeAPI, RequestID: "r1", Service: "sqs"},
	}
	got := injectedDeliveryFailures(hits)
	if len(got) != 1 || got[0].RequestID != "d1" || got[0].Delivery == nil || got[0].Delivery.Source != "sqs" {
		t.Fatalf("failures: %+v", got)
	}

	report := diagnose.Diagnose(got, map[string]diagnose.Injection{"d1": {RuleID: "boom", Action: "function_error"}}, diagnose.Options{})
	if !report.Healthy || report.Groups[0].Category != diagnose.CategoryInjected {
		t.Fatalf("report: %+v", report)
	}
}
