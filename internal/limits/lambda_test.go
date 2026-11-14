package limits

import "testing"

func invoke(fn string, reserved *int, reservedTotal int) FunctionRequest {
	return FunctionRequest{
		Request:  Request{RequestID: "req-" + fn, Account: "111111111111", Region: "us-east-1", Service: "lambda", Operation: "Invoke", Resource: fn},
		Reserved: reserved, ReservedTotal: reservedTotal,
	}
}

func intPtr(v int) *int { return &v }

func TestLambdaConcurrencyIsOffWithoutALambdaModel(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	for i := 0; i < 5; i++ {
		if _, b := g.AcquireFunction(invoke("f", intPtr(0), 0)); b != nil {
			t.Fatal("the none base enforced concurrency without a lambda section")
		}
	}
}

func TestReservedConcurrencyCapsAFunctionAndIsNotScaled(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Scale: 0.001})
	first, b := g.AcquireFunction(invoke("f", intPtr(2), 2))
	if b != nil {
		t.Fatal("first execution throttled")
	}
	if _, b := g.AcquireFunction(invoke("f", intPtr(2), 2)); b != nil {
		t.Fatal("second execution throttled; reserved concurrency must not be scaled")
	}
	_, b = g.AcquireFunction(invoke("f", intPtr(2), 2))
	if b == nil || b.Reason != ReasonReservedConcurrency || b.RuleID != RuleLambdaReservedConcurrency || b.Limit != 2 {
		t.Fatalf("third execution: %+v", b)
	}
	first()
	if _, b := g.AcquireFunction(invoke("f", intPtr(2), 2)); b != nil {
		t.Fatal("execution after a release was throttled")
	}
	// Reserved concurrency of zero stops the function entirely.
	if _, b := g.AcquireFunction(invoke("off", intPtr(0), 2)); b == nil {
		t.Fatal("a function with zero reserved concurrency ran")
	}
}

func TestUnreservedFunctionsShareTheScaledAccountPool(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Profile: BaseNone, Scale: 0.01, Lambda: &LambdaSpec{}})
	// 1000 * 0.01 = 10, less 7 reserved elsewhere, leaves 3 for everyone else.
	for i, fn := range []string{"a", "b", "a"} {
		if _, b := g.AcquireFunction(invoke(fn, nil, 7)); b != nil {
			t.Fatalf("execution %d throttled", i)
		}
	}
	_, b := g.AcquireFunction(invoke("c", nil, 7))
	if b == nil || b.Reason != ReasonAccountConcurrency || b.Limit != 3 {
		t.Fatalf("fourth execution: %+v", b)
	}
	if got := g.Breaches(); len(got) != 1 || got[0].Reason != ReasonAccountConcurrency {
		t.Fatalf("recorded breaches: %+v", got)
	}
}

func TestAccountPoolAlwaysAdmitsOne(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Profile: BaseNone, Lambda: &LambdaSpec{AccountConcurrency: 10}})
	if _, b := g.AcquireFunction(invoke("a", nil, 50)); b != nil {
		t.Fatal("an over-reserved account admitted nothing")
	}
}

func TestResolveLambda(t *testing.T) {
	def, _ := Resolve(Spec{}, known)
	if def.Lambda == nil || def.Lambda.AccountConcurrency != defaultAccountConcurrency {
		t.Fatalf("aws-default should enforce concurrency: %+v", def.Lambda)
	}
	off, _ := Resolve(Spec{Lambda: &LambdaSpec{Disabled: true}}, known)
	if off.Lambda != nil {
		t.Fatal("disabled lambda model still active")
	}
	if _, err := Resolve(Spec{Lambda: &LambdaSpec{AccountConcurrency: -1}}, known); controlCode(t, err) != "InvalidLimitsProfile" {
		t.Fatalf("negative concurrency: %v", err)
	}
	if p, err := Resolve(Spec{Profile: BaseNone, Lambda: &LambdaSpec{}}, known); err != nil || p.Lambda == nil {
		t.Fatalf("none with a lambda section: %+v, %v", p, err)
	}
}
