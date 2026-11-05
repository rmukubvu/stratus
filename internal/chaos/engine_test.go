package chaos

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stratus/internal/control"
)

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func newEngine() (*Engine, *manualClock) {
	clock := &manualClock{t: t0}
	return NewEngine([]string{"s3", "sqs", "lambda", "dynamodb"}, clock), clock
}

func mustStart(t *testing.T, e *Engine, spec Spec) Report {
	t.Helper()
	r, err := e.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return r
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func sqsSend(id string) Request {
	return Request{RequestID: id, Service: "sqs", Operation: "SendMessage", Path: "/"}
}

func TestNoExperimentMeansNoFaults(t *testing.T) {
	e, _ := newEngine()
	if d := e.Decide(sqsSend("r1")); d.Faulted() {
		t.Fatalf("unexpected fault: %+v", d)
	}
	_, err := e.Report()
	requireCode(t, err, "NoChaosExperiment")
	_, err = e.Stop()
	requireCode(t, err, "NoChaosExperiment")
}

func TestThrottleRuleUsesServiceSpecificErrors(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "*", Action: ActionThrottle}}})

	d := e.Decide(sqsSend("r1"))
	if d.Error == nil || d.Error.Code != "ThrottlingException" || d.Error.StatusCode != 400 {
		t.Fatalf("sqs throttle: %+v", d.Error)
	}
	d = e.Decide(Request{Service: "s3", Operation: "PutObject", Bucket: "assets"})
	if d.Error == nil || d.Error.Code != "SlowDown" || d.Error.StatusCode != 503 {
		t.Fatalf("s3 throttle: %+v", d.Error)
	}
}

func TestRulesMatchServiceOperationAndResource(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{
		{ID: "orders-put", Service: "dynamodb", Operation: "putitem", Resource: "orders", Action: ActionInternalError},
		{ID: "fn", Service: "lambda", Resource: "checkout", Action: ActionUnavailable},
		{ID: "bucket", Service: "s3", Resource: "uploads", Action: ActionDrop},
	}})

	cases := []struct {
		req  Request
		want []string
	}{
		{Request{Service: "dynamodb", Operation: "PutItem", Path: "/tables/orders"}, []string{"orders-put"}},
		{Request{Service: "dynamodb", Operation: "GetItem", Path: "/tables/orders"}, nil},
		{Request{Service: "dynamodb", Operation: "PutItem", Path: "/tables/users"}, nil},
		{Request{Service: "lambda", Operation: "Invoke", Function: "checkout-fn"}, []string{"fn"}},
		{Request{Service: "s3", Operation: "PutObject", Bucket: "uploads"}, []string{"bucket"}},
		{Request{Service: "sqs", Operation: "SendMessage"}, nil},
	}
	for _, tc := range cases {
		if got := e.Decide(tc.req).RuleIDs; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: fired %v, want %v", tc.req, got, tc.want)
		}
	}
}

func TestSameSeedReplaysSameFaults(t *testing.T) {
	spec := Spec{Seed: 42, Rules: []Rule{{Service: "sqs", Probability: 0.3, Action: ActionThrottle}}}
	run := func() []bool {
		e, _ := newEngine()
		mustStart(t, e, spec)
		out := make([]bool, 200)
		for i := range out {
			out[i] = e.Decide(sqsSend("r")).Faulted()
		}
		return out
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different faults")
	}

	hits := 0
	for _, f := range a {
		if f {
			hits++
		}
	}
	if hits < 40 || hits > 80 { // 30% of 200 = 60; generous bounds, deterministic anyway
		t.Fatalf("probability 0.3 fired %d/200 times", hits)
	}

	spec.Seed = 43
	e, _ := newEngine()
	mustStart(t, e, spec)
	differ := false
	for i := range a {
		if e.Decide(sqsSend("r")).Faulted() != a[i] {
			differ = true
		}
	}
	if !differ {
		t.Fatal("different seeds produced identical faults")
	}
}

func TestZeroSeedIsReplacedAndReported(t *testing.T) {
	e, _ := newEngine()
	r := mustStart(t, e, Spec{Rules: []Rule{{Service: "sqs", Action: ActionDrop}}})
	if r.Seed == 0 {
		t.Fatal("seed should be generated and reported for replay")
	}
}

func TestMaxHitsCapsInjections(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{ID: "twice", Service: "sqs", MaxHits: 2, Action: ActionThrottle}}})
	fired := 0
	for i := 0; i < 5; i++ {
		if e.Decide(sqsSend("r")).Faulted() {
			fired++
		}
	}
	r, _ := e.Report()
	if fired != 2 || r.Rules[0].Injected != 2 || r.Rules[0].Matched != 5 {
		t.Fatalf("fired=%d stats=%+v", fired, r.Rules[0])
	}
}

func TestLatencyAccumulatesAndErrorStopsEvaluation(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Seed: 7, Rules: []Rule{
		{ID: "slow", Service: "sqs", Action: ActionLatency, Latency: &LatencySpec{DelayMS: 100, JitterMS: 50}},
		{ID: "fail", Service: "sqs", Action: ActionUnavailable},
		{ID: "never", Service: "sqs", Action: ActionThrottle},
	}})
	d := e.Decide(sqsSend("r1"))
	if d.Delay < 100*time.Millisecond || d.Delay > 150*time.Millisecond {
		t.Fatalf("delay %s outside [100ms,150ms]", d.Delay)
	}
	if d.Error == nil || d.Error.Code != "ServiceUnavailable" || !reflect.DeepEqual(d.RuleIDs, []string{"slow", "fail"}) {
		t.Fatalf("decision: %+v", d)
	}
	r, _ := e.Report()
	if r.Rules[2].Matched != 0 {
		t.Fatal("rules after a terminal fault must not count the request")
	}
}

func TestColdStartTargetsInvokedFunction(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "lambda", Operation: "Invoke", Action: ActionColdStart}}})
	d := e.Decide(Request{Service: "lambda", Operation: "Invoke", Function: "checkout"})
	if d.ColdStart != "checkout" || d.Error != nil || d.Drop {
		t.Fatalf("decision: %+v", d)
	}
}

func TestExperimentExpiresAfterDuration(t *testing.T) {
	e, clock := newEngine()
	r := mustStart(t, e, Spec{DurationSeconds: 60, Rules: []Rule{{Service: "sqs", Action: ActionDrop}}})
	if r.EndsAt == nil || !r.EndsAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("ends_at: %v", r.EndsAt)
	}
	if !e.Decide(sqsSend("r")).Faulted() {
		t.Fatal("should fault while running")
	}
	clock.t = t0.Add(time.Minute)
	if e.Decide(sqsSend("r")).Faulted() {
		t.Fatal("should not fault after expiry")
	}
	if r, _ := e.Report(); r.Status != StatusExpired {
		t.Fatalf("status %s", r.Status)
	}
}

func TestReportRecordsHitsForDiagnosis(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{ID: "t", Service: "sqs", Action: ActionThrottle}}})
	e.Decide(sqsSend("req-1"))
	e.Decide(sqsSend("req-2"))

	final, err := e.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusStopped || final.TotalInjected != 2 || len(final.RecentHits) != 2 {
		t.Fatalf("report: %+v", final)
	}
	h := final.RecentHits[1]
	if h.RequestID != "req-2" || h.RuleID != "t" || h.Match != 2 || h.Operation != "SendMessage" {
		t.Fatalf("hit: %+v", h)
	}
	if e.Decide(sqsSend("r")).Faulted() {
		t.Fatal("stopped experiment still faulting")
	}
}

func TestHitLogIsBounded(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "sqs", Action: ActionThrottle}}})
	for i := 0; i < hitLogSize+10; i++ {
		e.Decide(sqsSend("r"))
	}
	r, _ := e.Report()
	if len(r.RecentHits) != hitLogSize || r.RecentHits[len(r.RecentHits)-1].Match != hitLogSize+10 {
		t.Fatalf("hits=%d last=%+v", len(r.RecentHits), r.RecentHits[len(r.RecentHits)-1])
	}
}

func TestStartReplacesPreviousExperiment(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "sqs", Action: ActionThrottle}}})
	mustStart(t, e, Spec{Rules: []Rule{{Service: "s3", Action: ActionThrottle}}})
	if e.Decide(sqsSend("r")).Faulted() {
		t.Fatal("old experiment still active")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]Spec{
		"no rules":          {},
		"unknown service":   {Rules: []Rule{{Service: "s4", Action: ActionThrottle}}},
		"missing service":   {Rules: []Rule{{Action: ActionThrottle}}},
		"bad probability":   {Rules: []Rule{{Service: "s3", Probability: 1.5, Action: ActionThrottle}}},
		"unknown action":    {Rules: []Rule{{Service: "s3", Action: "explode"}}},
		"error w/o spec":    {Rules: []Rule{{Service: "s3", Action: ActionError}}},
		"error bad status":  {Rules: []Rule{{Service: "s3", Action: ActionError, Error: &ErrorSpec{StatusCode: 200, Code: "X"}}}},
		"latency w/o delay": {Rules: []Rule{{Service: "s3", Action: ActionLatency}}},
		"latency too long":  {Rules: []Rule{{Service: "s3", Action: ActionLatency, Latency: &LatencySpec{DelayMS: 600000}}}},
		"cold start on s3":  {Rules: []Rule{{Service: "s3", Action: ActionColdStart}}},
		"duplicate ids":     {Rules: []Rule{{ID: "a", Service: "s3", Action: ActionDrop}, {ID: "a", Service: "s3", Action: ActionDrop}}},
		"negative duration": {DurationSeconds: -1, Rules: []Rule{{Service: "s3", Action: ActionDrop}}},
	}
	for name, spec := range cases {
		e, _ := newEngine()
		_, err := e.Start(spec)
		var ce *control.Error
		if !errors.As(err, &ce) || ce.Kind != control.KindInvalid || ce.Hint == "" {
			t.Errorf("%s: expected invalid error with hint, got %v", name, err)
		}
	}
}

func TestCustomErrorGetsDefaultMessage(t *testing.T) {
	e, _ := newEngine()
	mustStart(t, e, Spec{Rules: []Rule{{Service: "dynamodb", Action: ActionError,
		Error: &ErrorSpec{StatusCode: 400, Code: "ProvisionedThroughputExceededException"}}}})
	d := e.Decide(Request{Service: "dynamodb", Operation: "PutItem"})
	if d.Error.Code != "ProvisionedThroughputExceededException" || d.Error.Message == "" {
		t.Fatalf("error: %+v", d.Error)
	}
}
