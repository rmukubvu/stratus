package diagnose

import (
	"strings"
	"testing"
	"time"
)

func deliveryFailure(id string, at time.Duration, code, message string, status int) Failure {
	return Failure{
		Time: t0.Add(at), RequestID: id, Service: "lambda", Operation: "Invoke",
		Path: "arn:aws:lambda:us-east-1:000000000000:function:process-order", Code: code, Message: message, Status: status,
		Delivery: &Delivery{Source: "sqs", SourceARN: "arn:aws:sqs:us-east-1:000000000000:orders", TargetARN: "arn:aws:lambda:us-east-1:000000000000:function:process-order"},
	}
}

func TestFunctionErrorsInDeliveriesAreTheCallersBug(t *testing.T) {
	r := Diagnose([]Failure{
		deliveryFailure("e1", time.Second, FunctionErrorCode, "TypeError: Float types are not supported", 200),
		deliveryFailure("e2", 2*time.Second, FunctionErrorCode, "TypeError: Float types are not supported", 200),
	}, nil, Options{})

	if r.Healthy || r.Failures != 2 || len(r.Groups) != 1 {
		t.Fatalf("report: %+v", r)
	}
	g := r.Groups[0]
	if g.Category != CategoryFunctionError || !g.YourBug || g.EventSource != "sqs" {
		t.Fatalf("group: %+v", g)
	}
	for _, want := range []string{"process-order", "from sqs", "Float types"} {
		if !strings.Contains(g.LikelyCause, want) {
			t.Errorf("likely cause %q does not mention %q", g.LikelyCause, want)
		}
	}
	if !strings.Contains(g.NextStep, "aws logs filter-log-events --log-group-name /aws/lambda/process-order") {
		t.Errorf("next step should point at the function's logs: %q", g.NextStep)
	}
	if !strings.Contains(r.Summary, "2 function error") {
		t.Errorf("summary: %q", r.Summary)
	}
	if !strings.Contains(g.NextStep, "stays on the queue") || strings.Contains(g.NextStep, "resend") {
		t.Errorf("a queue retries the batch itself, so the next step should not say to resend it: %q", g.NextStep)
	}
}

func TestFunctionErrorsFromPushSourcesSayToResend(t *testing.T) {
	f := deliveryFailure("e1", time.Second, FunctionErrorCode, "boom", 200)
	f.Delivery.Source = "sns"
	if exp := explainDelivery(f); !strings.Contains(exp.NextStep, "resend the event") {
		t.Fatalf("next step: %q", exp.NextStep)
	}
}

func TestInjectedDeliveryFaultsAreSeparatedFromRealOnes(t *testing.T) {
	failures := []Failure{
		deliveryFailure("real", time.Second, FunctionErrorCode, "boom", 200),
		deliveryFailure("chaos", 2*time.Second, "InjectedFunctionError", "injected", 200),
	}
	injected := map[string]Injection{"chaos": {RuleID: "sqs-to-lambda", Action: "function_error"}}

	r := Diagnose(failures, injected, Options{})
	if len(r.Groups) != 2 || r.Groups[0].Category != CategoryFunctionError || r.Groups[1].Category != CategoryInjected {
		t.Fatalf("groups: %+v", r.Groups)
	}
	if r.ByCause[CategoryInjected] != 1 || r.ByCause[CategoryFunctionError] != 1 {
		t.Fatalf("by cause: %+v", r.ByCause)
	}
	if only := Diagnose(failures, injected, Options{ExcludeInjected: true}); only.Failures != 1 {
		t.Fatalf("exclude_injected kept %d failures", only.Failures)
	}
}

func TestUndeliverableEventsExplainTheEnvironment(t *testing.T) {
	r := Diagnose([]Failure{
		deliveryFailure("d1", time.Second, "ServiceException", "lambda image is not available locally: public.ecr.aws/lambda/python:3.12 (run: docker pull public.ecr.aws/lambda/python:3.12)", 503),
	}, nil, Options{})
	g := r.Groups[0]
	if g.Category != CategoryEnvironment || g.YourBug {
		t.Fatalf("group: %+v", g)
	}
	if !strings.Contains(g.LikelyCause, "could not deliver an event from sqs") || !strings.Contains(g.LikelyCause, "docker pull") {
		t.Errorf("likely cause: %q", g.LikelyCause)
	}
	if !strings.Contains(g.NextStep, "nothing upstream reports this failure") {
		t.Errorf("next step: %q", g.NextStep)
	}
}

func TestAPIAndDeliveryFailuresWithTheSameCodeAreGroupedApart(t *testing.T) {
	api := deliveryFailure("api", time.Second, "ServiceException", "docker is not running", 503)
	api.Delivery = nil
	event := deliveryFailure("event", 2*time.Second, "ServiceException", "docker is not running", 503)
	if r := Diagnose([]Failure{api, event}, nil, Options{}); len(r.Groups) != 2 {
		t.Fatalf("want separate groups, got %+v", r.Groups)
	}
}

func TestDeliveryLogKeepsTheNewestFailures(t *testing.T) {
	log := NewDeliveryLog(2)
	for _, id := range []string{"a", "b", "c"} {
		log.Record(Failure{RequestID: id})
	}
	got := log.Failures()
	if len(got) != 2 || got[0].RequestID != "c" || got[1].RequestID != "b" {
		t.Fatalf("failures: %+v", got)
	}
	if got[0].Time.IsZero() {
		t.Fatal("a failure without a time should be stamped")
	}
}
