package chaos

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fixedDecider struct {
	decision Decision
	seen     []Delivery
}

func (f *fixedDecider) DecideDelivery(d Delivery) Decision {
	f.seen = append(f.seen, d)
	return f.decision
}

type injectorProbe struct {
	injector  *Injector
	slept     []time.Duration
	spawned   []func()
	coldStart []string
	calls     int
}

func newProbe(d Decision) *injectorProbe {
	p := &injectorProbe{}
	p.injector = NewInjector(&fixedDecider{decision: d}, func(_ context.Context, fn string) error {
		p.coldStart = append(p.coldStart, fn)
		return nil
	}, nil)
	p.injector.sleep = func(_ context.Context, d time.Duration) error {
		p.slept = append(p.slept, d)
		return nil
	}
	p.injector.spawn = func(f func()) { p.spawned = append(p.spawned, f) }
	return p
}

func (p *injectorProbe) deliver(async bool) error {
	return p.injector.Deliver(context.Background(), Delivery{Service: "lambda", Function: "checkout", Async: async}, func(context.Context) error {
		p.calls++
		return nil
	})
}

func TestPassthroughDelivers(t *testing.T) {
	calls := 0
	err := Passthrough{}.Deliver(context.Background(), Delivery{}, func(context.Context) error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestInjectorWithoutFaultDeliversOnce(t *testing.T) {
	p := newProbe(Decision{})
	if err := p.deliver(true); err != nil || p.calls != 1 {
		t.Fatalf("calls=%d err=%v", p.calls, err)
	}
}

func TestInjectorDropSkipsDelivery(t *testing.T) {
	for _, async := range []bool{true, false} {
		p := newProbe(Decision{RuleIDs: []string{"lose"}, Drop: true})
		if err := p.deliver(async); err != nil || p.calls != 0 {
			t.Fatalf("async=%v: calls=%d err=%v", async, p.calls, err)
		}
	}
}

func TestInjectorErrorFailsSyncDeliveries(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"boom"}, Error: &ErrorSpec{StatusCode: 500, Code: "ServiceException", Message: "x"}})
	err := p.deliver(false)
	var fault *FaultError
	if !errors.As(err, &fault) || fault.Spec.Code != "ServiceException" || fault.RuleIDs[0] != "boom" {
		t.Fatalf("expected a FaultError, got %v", err)
	}
	if p.calls != 0 {
		t.Fatal("a failed delivery must not reach the target")
	}
}

func TestInjectorFunctionErrorFailsSyncDeliveries(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"throw"}, FunctionError: "boom"})
	var fault *FaultError
	if err := p.deliver(false); !errors.As(err, &fault) || fault.Spec.Code != FunctionErrorType || p.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, p.calls)
	}
}

func TestInjectorErrorIsAbsorbedForAsyncDeliveries(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"boom"}, Error: &ErrorSpec{StatusCode: 500, Code: "InternalFailure"}})
	if err := p.deliver(true); err != nil || p.calls != 0 {
		t.Fatalf("calls=%d err=%v", p.calls, err)
	}
}

func TestInjectorDuplicateDeliversTwice(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"twice"}, Duplicate: true})
	if err := p.deliver(true); err != nil || p.calls != 2 {
		t.Fatalf("calls=%d err=%v", p.calls, err)
	}
}

func TestInjectorDelaysSyncDeliveriesInline(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"slow"}, Delay: 300 * time.Millisecond})
	if err := p.deliver(false); err != nil {
		t.Fatal(err)
	}
	if len(p.spawned) != 0 || len(p.slept) != 1 || p.slept[0] != 300*time.Millisecond || p.calls != 1 {
		t.Fatalf("spawned=%d slept=%v calls=%d", len(p.spawned), p.slept, p.calls)
	}
}

func TestInjectorDelaysAsyncDeliveriesInBackground(t *testing.T) {
	p := newProbe(Decision{RuleIDs: []string{"slow"}, Delay: time.Second, ColdStart: "checkout"})
	if err := p.deliver(true); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || len(p.spawned) != 1 {
		t.Fatalf("the source must not wait: calls=%d spawned=%d", p.calls, len(p.spawned))
	}
	p.spawned[0]()
	if p.calls != 1 || len(p.slept) != 1 || len(p.coldStart) != 1 || p.coldStart[0] != "checkout" {
		t.Fatalf("calls=%d slept=%v coldStart=%v", p.calls, p.slept, p.coldStart)
	}
}

func TestInjectorPropagatesTargetErrors(t *testing.T) {
	p := newProbe(Decision{})
	want := errors.New("queue does not exist")
	err := p.injector.Deliver(context.Background(), Delivery{Async: true}, func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("real target errors must reach the source, got %v", err)
	}
}

func TestInjectorWithEngineEndToEnd(t *testing.T) {
	e, _ := newEventEngine()
	mustStart(t, e, Spec{Seed: 7, Rules: []Rule{{Source: "sns", Service: "sqs", MaxHits: 1, Action: ActionDrop}}})
	injector := NewInjector(e, nil, nil)

	delivered := 0
	for range 3 {
		_ = injector.Deliver(context.Background(), snsToQueue("audit"), func(context.Context) error { delivered++; return nil })
	}
	if delivered != 2 {
		t.Fatalf("want the first delivery lost and the rest delivered, got %d delivered", delivered)
	}
}
