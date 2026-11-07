package chaos

import (
	"testing"
	"time"
)

func snsToQueue(queue string) Delivery {
	return Delivery{
		ID: "m1", Source: "sns", SourceARN: "arn:aws:sns:us-east-1:000000000000:orders",
		Service: "sqs", Operation: "SendMessage", TargetARN: "arn:aws:sqs:us-east-1:000000000000:" + queue, Async: true,
	}
}

func newEventEngine() (*Engine, *manualClock) {
	clock := &manualClock{t: t0}
	return NewEngine([]string{"s3", "sqs", "sns", "lambda", "events", "stepfunctions"}, clock), clock
}

func TestRulesDefaultToAPIScope(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "*", Action: ActionDrop}}})

	if d := e.DecideDelivery(snsToQueue("audit")); d.Faulted() {
		t.Fatalf("an api rule must not fault deliveries: %+v", d)
	}
	if d := e.Decide(sqsSend("r1")); !d.Drop {
		t.Fatalf("an api rule must still fault requests: %+v", d)
	}
}

func TestEventScopeMatchesOnlyDeliveries(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Scope: ScopeEvent, Service: "sqs", Action: ActionDrop}}})

	if d := e.Decide(sqsSend("r1")); d.Faulted() {
		t.Fatalf("an event rule must not fault requests: %+v", d)
	}
	if d := e.DecideDelivery(snsToQueue("audit")); !d.Drop {
		t.Fatalf("expected drop, got %+v", d)
	}
}

func TestScopeAllMatchesBoth(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Scope: ScopeAll, Service: "sqs", Operation: "SendMessage", Action: ActionInternalError}}})

	if d := e.Decide(sqsSend("r1")); d.Error == nil {
		t.Fatal("expected the request to fail")
	}
	if d := e.DecideDelivery(snsToQueue("audit")); d.Error == nil {
		t.Fatal("expected the delivery to fail")
	}
}

func TestSourceAndResourceNarrowDeliveries(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{
		{ID: "from-sns", Source: "sns", Service: "sqs", Resource: "audit", Action: ActionDuplicate},
	}})
	report, _ := e.Report()
	if report.Rules[0].Rule.Scope != ScopeEvent {
		t.Fatalf("source should imply event scope, got %q", report.Rules[0].Rule.Scope)
	}

	fromEvents := snsToQueue("audit")
	fromEvents.Source = "events"
	if d := e.DecideDelivery(fromEvents); d.Faulted() {
		t.Fatalf("other sources must not match: %+v", d)
	}
	if d := e.DecideDelivery(snsToQueue("billing")); d.Faulted() {
		t.Fatalf("other queues must not match: %+v", d)
	}
	if d := e.DecideDelivery(snsToQueue("audit")); !d.Duplicate {
		t.Fatalf("expected duplicate, got %+v", d)
	}
	// The source ARN is matched by resource too.
	mustStart(t, e, Spec{Rules: []Rule{{Source: "sns", Service: "*", Resource: ":orders", Action: ActionDrop}}})
	if d := e.DecideDelivery(snsToQueue("billing")); !d.Drop {
		t.Fatalf("expected the topic ARN to match: %+v", d)
	}
}

func TestDeliveryHitsRecordScopeAndSource(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{{ID: "lose", Scope: ScopeEvent, Service: "sqs", Action: ActionDrop}}})
	e.DecideDelivery(snsToQueue("audit"))

	hits := e.Hits()
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	h := hits[0]
	if h.Scope != ScopeEvent || h.Source != "sns" || h.SourceARN == "" || h.RequestID != "m1" ||
		h.Path != "arn:aws:sqs:us-east-1:000000000000:audit" || h.Operation != "SendMessage" {
		t.Fatalf("unexpected hit: %+v", h)
	}
}

func TestDeliveriesReplayWithSameSeed(t *testing.T) {
	run := func() []bool {
		e, _ := newEventEngine()
		mustStart(t, e, Spec{Seed: 99, Rules: []Rule{{Scope: ScopeEvent, Service: "sqs", Probability: 0.4, Action: ActionDrop}}})
		out := make([]bool, 40)
		for i := range out {
			out[i] = e.DecideDelivery(snsToQueue("audit")).Drop
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("delivery %d differs between runs", i+1)
		}
	}
}

func TestRuleWindows(t *testing.T) {
	e, clock := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{
		{ID: "later", Service: "sqs", StartAfterSeconds: 10, DurationSeconds: 5, Action: ActionThrottle},
	}})

	if d := e.Decide(sqsSend("r1")); d.Faulted() {
		t.Fatal("rule must not fire before it starts")
	}
	clock.t = t0.Add(10 * time.Second)
	if d := e.Decide(sqsSend("r2")); !d.Faulted() {
		t.Fatal("rule must fire once it has started")
	}
	clock.t = t0.Add(15 * time.Second)
	if d := e.Decide(sqsSend("r3")); d.Faulted() {
		t.Fatal("rule must stop after its duration")
	}
	report, _ := e.Report()
	if got := report.Rules[0].Matched; got != 1 {
		t.Fatalf("inactive rules must not count matches, got %d", got)
	}
}

func TestEventRuleValidation(t *testing.T) {
	e, _ := newEventEngine()
	cases := map[string]Rule{
		"bad scope":           {Service: "sqs", Scope: "sometimes", Action: ActionDrop},
		"source on api scope": {Service: "sqs", Scope: ScopeAPI, Source: "sns", Action: ActionDrop},
		"unknown source":      {Service: "sqs", Source: "nope", Action: ActionDrop},
		"duplicate on api":    {Service: "sqs", Action: ActionDuplicate},
		"duplicate on all":    {Service: "sqs", Scope: ScopeAll, Action: ActionDuplicate},
		"negative window":     {Service: "sqs", StartAfterSeconds: -1, Action: ActionDrop},
	}
	for name, rule := range cases {
		_, err := e.Start(Spec{Rules: []Rule{rule}})
		if err == nil {
			t.Errorf("%s: expected a validation error", name)
			continue
		}
		requireCode(t, err, "InvalidRule")
	}
}

func TestStopIDOnlyStopsThatExperiment(t *testing.T) {
	e, _ := newEventEngine()
	first := mustStart(t, e, Spec{Rules: []Rule{{Service: "sqs", Action: ActionDrop}}})
	second := mustStart(t, e, Spec{Rules: []Rule{{Service: "sqs", Action: ActionDrop}}})
	if first.ID == "" || first.ID == second.ID {
		t.Fatalf("experiments need distinct IDs: %q, %q", first.ID, second.ID)
	}

	_, err := e.StopID(first.ID)
	requireCode(t, err, "NoChaosExperiment")
	report, err := e.StopID(second.ID)
	if err != nil || report.Status != StatusStopped {
		t.Fatalf("stop: %+v, %v", report, err)
	}
}

func TestFunctionErrorTargetsLambdaInvoke(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Rules: []Rule{{ID: "fail", Scope: ScopeAll, Service: "lambda", Resource: "checkout", Action: ActionFunctionError}}})

	report, _ := e.Report()
	if report.Rules[0].Rule.Operation != "Invoke" {
		t.Fatalf("function_error should pin the operation to Invoke, got %q", report.Rules[0].Rule.Operation)
	}
	if d := e.Decide(Request{Service: "lambda", Operation: "GetFunction", Function: "checkout"}); d.Faulted() {
		t.Fatal("only Invoke can fail with a function error")
	}
	d := e.Decide(Request{Service: "lambda", Operation: "Invoke", Function: "checkout"})
	if d.FunctionError == "" {
		t.Fatalf("expected a function error, got %+v", d)
	}
	invoke := Delivery{Source: "stepfunctions", Service: "lambda", Operation: "Invoke", Function: "checkout"}
	if d := e.DecideDelivery(invoke); d.FunctionError == "" {
		t.Fatalf("expected a function error on the delivery, got %+v", d)
	}

	for _, bad := range []Rule{
		{Service: "sqs", Action: ActionFunctionError},
		{Service: "lambda", Operation: "GetFunction", Action: ActionFunctionError},
	} {
		_, err := e.Start(Spec{Rules: []Rule{bad}})
		requireCode(t, err, "InvalidRule")
	}
}
