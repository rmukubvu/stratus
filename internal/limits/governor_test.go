package limits

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func ssmRequest(id string) Request {
	return Request{RequestID: id, Account: "111111111111", Region: "us-east-1", Service: "ssm", Operation: "GetParameter"}
}

// onlyRule is a profile with one rate rule and no base rules.
func onlyRule(r RateRule) Spec { return Spec{Profile: BaseNone, Rates: []RateRule{r}} }

func admitN(g *Governor, req Request, n int) (admitted, throttled int) {
	for i := 0; i < n; i++ {
		if g.Admit(req) == nil {
			admitted++
		} else {
			throttled++
		}
	}
	return admitted, throttled
}

func TestGovernorIsOffUntilAProfileIsApplied(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	if g.Enabled() {
		t.Fatal("enabled before Apply")
	}
	if _, throttled := admitN(g, ssmRequest("r"), 1000); throttled != 0 {
		t.Fatalf("throttled %d requests with no profile", throttled)
	}
	if _, err := g.Status(); err == nil {
		t.Fatal("Status with no profile should fail")
	}
}

func TestRateRuleAdmitsBurstThenRefills(t *testing.T) {
	clock := newFakeClock()
	g := NewGovernor(nil, clock)
	if _, err := g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 10})); err != nil {
		t.Fatal(err)
	}

	admitted, throttled := admitN(g, ssmRequest("r"), 15)
	if admitted != 10 || throttled != 5 {
		t.Fatalf("first second: admitted %d throttled %d, want 10 and 5", admitted, throttled)
	}
	clock.Advance(500 * time.Millisecond)
	if admitted, _ := admitN(g, ssmRequest("r"), 10); admitted != 5 {
		t.Fatalf("after half a second: admitted %d, want 5", admitted)
	}

	status, _ := g.Status()
	if got := status.Rules[0]; got.Admitted != 15 || got.Throttled != 10 || status.TotalThrottled != 10 {
		t.Fatalf("unexpected counters: %+v", status)
	}
}

func TestBreachDescribesTheRequest(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	g.Admit(ssmRequest("first"))
	b := g.Admit(ssmRequest("second"))
	if b == nil {
		t.Fatal("second request was not throttled")
	}
	if b.RuleID != "ssm" || b.Kind != KindRate || b.RequestID != "second" || b.Limit != 1 || b.Key != "ssm/111111111111/us-east-1" {
		t.Fatalf("unexpected breach: %+v", b)
	}
	if got := g.Breaches(); len(got) != 1 || got[0].RequestID != "second" {
		t.Fatalf("breach log: %+v", got)
	}
}

func TestScaleShrinksRatesButAlwaysAdmitsOne(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Profile: BaseNone, Scale: 0.1, Rates: []RateRule{{ID: "ssm", Service: "ssm", PerSecond: 40}}})
	if admitted, _ := admitN(g, ssmRequest("r"), 10); admitted != 4 {
		t.Fatalf("scale 0.1 of 40/s admitted %d, want 4", admitted)
	}

	_, _ = g.Apply(Spec{Profile: BaseNone, Scale: 0.001, Rates: []RateRule{{ID: "ssm", Service: "ssm", PerSecond: 40}}})
	if admitted, _ := admitN(g, ssmRequest("r"), 10); admitted != 1 {
		t.Fatalf("a tiny scale admitted %d, want 1", admitted)
	}
}

func TestAccountsAndRegionsHaveSeparateBuckets(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	a := ssmRequest("a")
	b := ssmRequest("b")
	b.Account = "222222222222"
	c := ssmRequest("c")
	c.Region = "eu-west-1"
	for _, req := range []Request{a, b, c} {
		if g.Admit(req) != nil {
			t.Fatalf("first request in %s/%s throttled", req.Account, req.Region)
		}
	}
	if g.Admit(a) == nil {
		t.Fatal("second request in the first account was admitted")
	}
}

func TestPrefixScopeGivesEachPrefixItsOwnBucket(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "s3", Service: "s3", Per: ScopePrefix, PerSecond: 2}))
	req := func(prefix string) Request {
		return Request{Account: "1", Region: "r", Service: "s3", Operation: "GetObject", Resource: "bucket", Prefix: prefix}
	}
	if admitted, _ := admitN(g, req("bucket/hot/"), 5); admitted != 2 {
		t.Fatalf("hot prefix admitted %d, want 2", admitted)
	}
	if admitted, _ := admitN(g, req("bucket/cold/"), 2); admitted != 2 {
		t.Fatalf("other prefix admitted %d, want 2", admitted)
	}
}

func TestOperationsFilterAndThrottledRequestsConsumeNothing(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Profile: BaseNone, Rates: []RateRule{
		{ID: "wide", Service: "ssm", PerSecond: 100},
		{ID: "narrow", Service: "ssm", Operations: []string{"GetParameter"}, PerSecond: 1},
	}})
	put := Request{Account: "1", Region: "r", Service: "ssm", Operation: "PutParameter"}
	if _, throttled := admitN(g, put, 50); throttled != 0 {
		t.Fatalf("PutParameter was throttled %d times by a GetParameter rule", throttled)
	}
	admitN(g, ssmRequest("r"), 10) // 1 admitted, 9 throttled by "narrow"
	status, _ := g.Status()
	wide := status.Rules[0]
	if wide.Admitted != 51 {
		t.Fatalf("wide admitted %d, want 51: throttled requests must not consume other buckets", wide.Admitted)
	}
	if narrow := status.Rules[1]; narrow.Admitted != 1 || narrow.Throttled != 9 {
		t.Fatalf("narrow: %+v", narrow)
	}
}

func TestClearTurnsLimitsOffAndKeepsBreaches(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	admitN(g, ssmRequest("r"), 3)
	final, err := g.Clear()
	if err != nil || final.TotalThrottled != 2 {
		t.Fatalf("Clear: %+v, %v", final, err)
	}
	if _, throttled := admitN(g, ssmRequest("r"), 10); throttled != 0 {
		t.Fatal("limits still applied after Clear")
	}
	if got := len(g.Breaches()); got != 2 {
		t.Fatalf("kept %d breaches after Clear, want 2", got)
	}
	if _, err := g.Clear(); err == nil {
		t.Fatal("second Clear should fail")
	}
}

func TestSinksReceiveBreaches(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	var got []Breach
	g.Subscribe(func(b Breach) { got = append(got, b) })
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	admitN(g, ssmRequest("r"), 3)
	if len(got) != 2 {
		t.Fatalf("sink got %d breaches, want 2", len(got))
	}
}

func TestAcquireCapsConcurrency(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	key := QuotaKey{RuleID: "lambda-account", Account: "1", Region: "r"}
	req := Request{Service: "lambda", Operation: "Invoke"}

	release, b := g.Acquire(key, 1, req)
	if b != nil {
		t.Fatal("first acquire with no profile failed")
	}
	release()

	_, _ = g.Apply(Spec{})
	first, b := g.Acquire(key, 2, req)
	if b != nil {
		t.Fatal("first acquire failed")
	}
	second, b := g.Acquire(key, 2, req)
	if b != nil {
		t.Fatal("second acquire failed")
	}
	if _, b := g.Acquire(key, 2, req); b == nil || b.Kind != KindConcurrency || b.Limit != 2 {
		t.Fatalf("third acquire: breach %+v", b)
	}
	first()
	first() // idempotent
	if _, b := g.Acquire(key, 2, req); b != nil {
		t.Fatal("acquire after release failed")
	}
	second()

	status, _ := g.Status()
	if len(status.Concurrency) != 1 || status.Concurrency[0].Admitted != 3 || status.Concurrency[0].Throttled != 1 {
		t.Fatalf("concurrency status: %+v", status.Concurrency)
	}
}

func TestAdmitIsSafeForConcurrentUse(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 100}))
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _ := admitN(g, ssmRequest("r"), 50)
			mu.Lock()
			admitted += a
			mu.Unlock()
		}()
	}
	wg.Wait()
	if admitted != 100 {
		t.Fatalf("admitted %d across goroutines, want exactly 100", admitted)
	}
}
