package fis

import (
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
)

func requireValidation(t *testing.T, err error, contains string) {
	t.Helper()
	ae, ok := err.(*apierror.Error)
	if !ok || ae.Code != "ValidationException" || !strings.Contains(ae.Message, contains) {
		t.Fatalf("want ValidationException containing %q, got %v", contains, err)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT5M":    5 * time.Minute,
		"PT1H30M": 90 * time.Minute,
		"PT90S":   90 * time.Second,
		"PT0.5S":  500 * time.Millisecond,
		"P1DT1S":  24*time.Hour + time.Second,
	}
	for in, want := range cases {
		got, ok := parseDuration(in)
		if !ok || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "5m", "P", "PT", "PT0S", "PTXM"} {
		if _, ok := parseDuration(bad); ok {
			t.Errorf("parseDuration(%q) should fail", bad)
		}
	}
}

func dur(d string) map[string]string { return map[string]string{"duration": d} }

func TestScheduleChainsStartAfter(t *testing.T) {
	p, err := schedule(map[string]Action{
		"warmup": {ActionID: "aws:fis:wait", Parameters: dur("PT1M")},
		"slow":   {ActionID: "aws:lambda:invocation-add-delay", Parameters: dur("PT2M"), StartAfter: []string{"warmup"}},
		"fail":   {ActionID: "aws:lambda:invocation-error", Parameters: dur("PT30S"), StartAfter: []string{"warmup", "slow"}},
	}, func(a Action) (time.Duration, error) { return params(a.Parameters).duration() })
	if err != nil {
		t.Fatal(err)
	}
	if w := p.Windows["slow"]; w.Start != time.Minute || w.Length != 2*time.Minute {
		t.Fatalf("slow window: %+v", w)
	}
	if w := p.Windows["fail"]; w.Start != 3*time.Minute {
		t.Fatalf("fail should start after both dependencies: %+v", w)
	}
	if p.Total != 3*time.Minute+30*time.Second {
		t.Fatalf("total: %v", p.Total)
	}
}

func TestScheduleRejectsBadGraphs(t *testing.T) {
	durationOf := func(a Action) (time.Duration, error) { return params(a.Parameters).duration() }
	_, err := schedule(map[string]Action{"a": {StartAfter: []string{"ghost"}}}, durationOf)
	requireValidation(t, err, "not an action")

	_, err = schedule(map[string]Action{
		"a": {Parameters: dur("PT1M"), StartAfter: []string{"b"}},
		"b": {Parameters: dur("PT1M"), StartAfter: []string{"a"}},
	}, durationOf)
	requireValidation(t, err, "cycle")

	_, err = schedule(map[string]Action{
		"forever": {},
		"after":   {Parameters: dur("PT1M"), StartAfter: []string{"forever"}},
	}, durationOf)
	requireValidation(t, err, "never finishes")
}

func TestScheduleWithOpenEndedActionHasNoTotal(t *testing.T) {
	p, err := schedule(map[string]Action{"a": {Parameters: dur("PT1M")}, "b": {}},
		func(a Action) (time.Duration, error) { return params(a.Parameters).duration() })
	if err != nil || p.Total != 0 {
		t.Fatalf("total=%v err=%v", p.Total, err)
	}
}

func lambdaTemplate(arns ...string) Template {
	return Template{
		Targets: map[string]Target{"fns": {ResourceType: ResourceLambdaFunction, ResourceArns: arns, SelectionMode: "ALL"}},
		Actions: map[string]Action{
			"fail": {ActionID: "aws:lambda:invocation-error", Targets: map[string]string{"Functions": "fns"},
				Parameters: map[string]string{"duration": "PT5M", "invocationPercentage": "25"}},
		},
	}
}

func TestCompileLambdaInvocationError(t *testing.T) {
	c, err := compile(lambdaTemplate(
		"arn:aws:lambda:us-east-1:000000000000:function:checkout",
		"arn:aws:lambda:us-east-1:000000000000:function:billing:live",
	), newCatalog(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if c.spec.Seed != 42 || c.spec.DurationSeconds != 300 || len(c.spec.Rules) != 2 {
		t.Fatalf("spec: %+v", c.spec)
	}
	r := c.spec.Rules[0]
	if r.ID != "fail/1" || r.Resource != "billing" || r.Scope != chaos.ScopeAll || r.Service != "lambda" ||
		r.Operation != "Invoke" || r.Action != chaos.ActionFunctionError || r.Probability != 0.25 || r.DurationSeconds != 300 {
		t.Fatalf("rule: %+v", r)
	}
}

func TestCompileAPIAndDeliveryActions(t *testing.T) {
	tmpl := Template{
		Targets: map[string]Target{"roles": {ResourceType: ResourceIAMRole, ResourceArns: []string{"arn:aws:iam::000000000000:role/app"}}},
		Actions: map[string]Action{
			"throttle": {ActionID: "aws:fis:inject-api-throttle-error", Targets: map[string]string{"Roles": "roles"},
				Parameters: map[string]string{"service": "dynamodb", "operations": "PutItem, GetItem", "percentage": "50", "duration": "PT1M"}},
			"dupes": {ActionID: "stratus:delivery:duplicate", StartAfter: []string{"throttle"},
				Parameters: map[string]string{"source": "sns", "service": "sqs", "resource": "audit", "duration": "PT1M"}},
		},
	}
	c, err := compile(tmpl, newCatalog(), 1)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]chaos.Rule{}
	for _, r := range c.spec.Rules {
		byID[r.ID] = r
	}
	if r := byID["throttle/2"]; r.Operation != "GetItem" || r.Service != "dynamodb" || r.Scope != chaos.ScopeAPI || r.Action != chaos.ActionThrottle {
		t.Fatalf("throttle rule: %+v", r)
	}
	if r := byID["dupes"]; r.Scope != chaos.ScopeEvent || r.Source != "sns" || r.Service != "sqs" ||
		r.Action != chaos.ActionDuplicate || r.StartAfterSeconds != 60 || r.DurationSeconds != 60 {
		t.Fatalf("dupes rule: %+v", r)
	}
	if c.spec.DurationSeconds != 120 {
		t.Fatalf("duration: %d", c.spec.DurationSeconds)
	}
	// Every compiled rule must be valid for the engine.
	if err := chaos.NewEngine(nil, nil).Validate(c.spec); err != nil {
		t.Fatalf("engine rejected compiled spec: %v", err)
	}
}

func TestCompileValidation(t *testing.T) {
	catalog := newCatalog()
	cases := map[string]struct {
		mutate func(*Template)
		want   string
	}{
		"no actions":       {func(t *Template) { t.Actions = nil }, "at least one action"},
		"unknown action":   {func(t *Template) { t.Actions["fail"] = Action{ActionID: "aws:ec2:stop-instances"} }, "not an action Stratus supports"},
		"unknown param":    {func(t *Template) { t.Actions["fail"].Parameters["colour"] = "red" }, "no parameter"},
		"undefined target": {func(t *Template) { t.Actions["fail"].Targets["Functions"] = "nope" }, "not defined"},
		"bad percentage":   {func(t *Template) { t.Actions["fail"].Parameters["invocationPercentage"] = "150" }, "invocationPercentage"},
		"bad duration":     {func(t *Template) { t.Actions["fail"].Parameters["duration"] = "5 minutes" }, "ISO 8601"},
		"tags selection": {func(t *Template) {
			t.Targets["fns"] = Target{ResourceType: ResourceLambdaFunction, ResourceTags: map[string]string{"team": "a"}}
		}, "resourceArns only"},
		"unsupported type": {func(t *Template) { t.Targets["fns"] = Target{ResourceType: "aws:ec2:instance"} }, "not supported"},
		"wrong type": {func(t *Template) {
			t.Targets["fns"] = Target{ResourceType: ResourceIAMRole}
		}, "needs aws:lambda:function"},
		"only waits": {func(t *Template) {
			t.Actions = map[string]Action{"w": {ActionID: "aws:fis:wait", Parameters: dur("PT1M")}}
		}, "only waits"},
		"wait without duration": {func(t *Template) {
			t.Actions["w"] = Action{ActionID: "aws:fis:wait"}
		}, "\"duration\" is required"},
		"missing service": {func(t *Template) {
			t.Actions["api"] = Action{ActionID: "aws:fis:inject-api-internal-error"}
		}, "\"service\" is required"},
	}
	for name, tc := range cases {
		tmpl := lambdaTemplate("arn:aws:lambda:us-east-1:000000000000:function:checkout")
		tc.mutate(&tmpl)
		_, err := compile(tmpl, catalog, 1)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		t.Run(name, func(t *testing.T) { requireValidation(t, err, tc.want) })
	}
}

func TestSelectTargets(t *testing.T) {
	arns := []string{
		"arn:aws:lambda:us-east-1:000000000000:function:c",
		"arn:aws:lambda:us-east-1:000000000000:function:a",
		"arn:aws:lambda:us-east-1:000000000000:function:b",
	}
	cases := map[string][]string{"ALL": {"a", "b", "c"}, "COUNT(2)": {"a", "b"}, "PERCENT(50)": {"a", "b"}, "COUNT(9)": {"a", "b", "c"}}
	for mode, want := range cases {
		got, err := selectTargets("t", Target{ResourceType: ResourceLambdaFunction, ResourceArns: arns, SelectionMode: mode})
		if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v, %v; want %v", mode, got, err, want)
		}
	}
	all, err := selectTargets("t", Target{ResourceType: ResourceLambdaFunction})
	if err != nil || all != nil {
		t.Fatalf("no ARNs should select every function: %v, %v", all, err)
	}
	_, err = selectTargets("t", Target{ResourceType: ResourceLambdaFunction, SelectionMode: "COUNT(1)"})
	requireValidation(t, err, "need resourceArns")
	_, err = selectTargets("t", Target{ResourceType: ResourceLambdaFunction, SelectionMode: "SOME"})
	requireValidation(t, err, "selectionMode")
}
