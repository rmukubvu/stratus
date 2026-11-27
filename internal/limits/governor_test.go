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

func TestRegionalRatesOverrideTheDefault(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	if _, err := g.Apply(onlyRule(RateRule{ID: "kms", Service: "kms", PerSecond: 10, RegionPerSecond: map[string]float64{"us-east-1": 100}})); err != nil {
		t.Fatal(err)
	}
	req := Request{Account: "111111111111", Region: "eu-north-1", Service: "kms", Operation: "Decrypt"}
	if admitted, _ := admitN(g, req, 200); admitted != 10 {
		t.Fatalf("eu-north-1 admitted %d, want the default 10", admitted)
	}
	req.Region = "us-east-1"
	admitted, _ := admitN(g, req, 200)
	if admitted != 100 {
		t.Fatalf("us-east-1 admitted %d, want its regional 100", admitted)
	}
	if b := g.Admit(req); b == nil || b.Limit != 100 {
		t.Fatalf("breach %+v should report the regional limit", b)
	}
	if _, err := g.Apply(onlyRule(RateRule{ID: "bad", Service: "kms", PerSecond: 1, RegionPerSecond: map[string]float64{"us-east-1": 0}})); err == nil {
		t.Fatal("accepted a zero regional rate")
	}
}

func TestAWSDefaultMatchesDocumentedQuotas(t *testing.T) {
	p, err := Resolve(Spec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"s3-reads-per-prefix": 5500, "s3-writes-per-prefix": 3500, "ssm-get-parameters": 40,
		"kms-cryptographic-operations": 10000, "secretsmanager-get-secret-value": 10000,
		"secretsmanager-describe-secret": 40000, "secretsmanager-batch-get-secret-value": 100,
		"sns-publish-rate": 300, "eventbridge-putevents-rate": 400,
	}
	got := map[string]RateRule{}
	for _, r := range p.Rates {
		got[r.ID] = r
	}
	for id, rate := range want {
		if got[id].PerSecond != rate {
			t.Errorf("%s: %v per second, want %v", id, got[id].PerSecond, rate)
		}
	}
	if len(got) != len(want) {
		t.Errorf("aws-default has %d rules, want %d", len(got), len(want))
	}
	kms := got["kms-cryptographic-operations"]
	if kms.RegionPerSecond["us-east-1"] != 100000 || kms.RegionPerSecond["eu-central-1"] != 20000 {
		t.Errorf("kms regional rates %v", kms.RegionPerSecond)
	}
	for _, op := range kms.Operations {
		if op == "Sign" || op == "GenerateDataKeyPair" {
			t.Errorf("kms symmetric rule covers %s, which has its own quota", op)
		}
	}
}

func TestMessagingRatesAreTheDocumentedQuotasTheAdvisorCites(t *testing.T) {
	p, err := Resolve(Spec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sns-publish-rate", "eventbridge-putevents-rate"} {
		q, _ := DocumentedQuota(id)
		var rule *RateRule
		for i := range p.Rates {
			if p.Rates[i].ID == id {
				rule = &p.Rates[i]
			}
		}
		if rule == nil {
			t.Fatalf("aws-default has no %s rule", id)
		}
		if rule.Service != q.Service || rule.PerSecond != q.Value || rule.Per != ScopeAccount {
			t.Errorf("%s: rule %+v does not match quota %+v", id, *rule, q)
		}
		for region, v := range q.RegionValues {
			if rule.RegionPerSecond[region] != v {
				t.Errorf("%s in %s: %v per second, want %v", id, region, rule.RegionPerSecond[region], v)
			}
		}
	}
}

func TestSNSPublishIsThrottledAtTheRegionsDocumentedRate(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	if _, err := g.Apply(onlyRule(documentedRate("sns-publish-rate", "Publish", "PublishBatch"))); err != nil {
		t.Fatal(err)
	}
	req := Request{Account: "111111111111", Region: "af-south-1", Service: "sns", Operation: "Publish"}
	if admitted, _ := admitN(g, req, 1000); admitted != 300 {
		t.Fatalf("af-south-1 admitted %d publishes in one second, want 300", admitted)
	}
	req.Operation = "PublishBatch"
	if b := g.Admit(req); b == nil || b.RuleID != "sns-publish-rate" {
		t.Fatalf("PublishBatch shares the bucket; got breach %+v", b)
	}
	req.Operation = "ListTopics"
	if b := g.Admit(req); b != nil {
		t.Fatalf("ListTopics is not a publish: %+v", b)
	}
	req.Region, req.Operation = "eu-west-1", "Publish"
	if admitted, _ := admitN(g, req, 10000); admitted != 9000 {
		t.Fatalf("eu-west-1 admitted %d, want 9,000", admitted)
	}
}

func TestEventBridgePutEventsIsThrottledPerAccount(t *testing.T) {
	clock := newFakeClock()
	g := NewGovernor(nil, clock)
	if _, err := g.Apply(onlyRule(documentedRate("eventbridge-putevents-rate", "PutEvents"))); err != nil {
		t.Fatal(err)
	}
	req := Request{Account: "111111111111", Region: "sa-east-1", Service: "events", Operation: "PutEvents"}
	if admitted, _ := admitN(g, req, 1000); admitted != 600 {
		t.Fatalf("sa-east-1 admitted %d, want 600", admitted)
	}
	other := req
	other.Account = "222222222222"
	if b := g.Admit(other); b != nil {
		t.Fatalf("another account has its own quota: %+v", b)
	}
	clock.Advance(time.Second)
	if admitted, _ := admitN(g, req, 1000); admitted != 600 {
		t.Fatalf("after a second admitted %d, want the refill of 600", admitted)
	}
}

func TestDocumentedRateRefusesAQuotaThatIsNotAnAccountRate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("documentedRate accepted a byte-size quota")
		}
	}()
	documentedRate("sqs-message-size")
}
