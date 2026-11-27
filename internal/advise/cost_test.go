package advise

import (
	"math"
	"strings"
	"testing"
)

// costedFamily is fakeFamily with a cost model: "simple" is fully priced,
// "heavy" needs a price the book does not have.
type costedFamily struct{ fakeFamily }

var fakeBook = PriceFunc(func(id, region string) (Price, bool) {
	if id != "requests" {
		return Price{}, false
	}
	return Price{ID: id, Unit: "requests", AsOf: "2026-11-26", Source: "https://pricing.example/offers/",
		Tiers: []PriceTier{{From: 0, To: 1e6, USD: 0}, {From: 1e6, USD: 1e-6}}}, true
})

func (costedFamily) Costs(r Requirements) []Estimate {
	qty := r.Number("rate") * 1e6
	return []Estimate{
		NewEstimate(fakeBook, "simple", "us-east-1").Add("Requests", "requests", qty).Assume("no batching").Build(),
		NewEstimate(fakeBook, "heavy", "us-east-1").Add("Requests", "requests", qty).Add("Brokers", "broker-hours", 730).Build(),
	}
}

func TestATieredPriceChargesEachUnitAtItsBand(t *testing.T) {
	p, _ := fakeBook.Price("requests", "us-east-1")
	for qty, want := range map[float64]float64{0: 0, 1e6: 0, 3e6: 2} {
		if got := p.Cost(qty); got != want {
			t.Errorf("Cost(%v) = %v, want %v", qty, got, want)
		}
	}
	sqs := Price{Tiers: []PriceTier{{From: 0, To: 100e9, USD: 0.4e-6}, {From: 100e9, To: 200e9, USD: 0.3e-6}, {From: 200e9, USD: 0.24e-6}}}
	// 250 billion: 100bn at $0.40, 100bn at $0.30 and 50bn at $0.24 a million.
	if got, want := sqs.Cost(250e9), 40_000.0+30_000+12_000; math.Abs(got-want) > 1e-6 {
		t.Errorf("Cost(250e9) = %v, want %v", got, want)
	}
}

func TestAssessAttachesEachOptionsEstimateAndKeepsItThroughTheOpsCheck(t *testing.T) {
	f := costedFamily{}
	d, err := NewDecision("adr-c", "Costed", f, t0)
	if err != nil {
		t.Fatal(err)
	}
	// A team with no ops capacity adds an ops caveat to "heavy", which
	// re-judges it; its estimate must survive that.
	if err := d.Answer(f, map[string]any{"rate": 5.0, OpsCapacityID: "none"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Assess(f, t0); err != nil {
		t.Fatal(err)
	}
	costs := map[string]*Estimate{}
	for _, a := range d.Assessments {
		costs[a.Option] = a.Cost
	}
	if c := costs["simple"]; c == nil || c.MonthlyUSD != 4 || !c.Complete() {
		t.Errorf("simple: %+v, want $4 and complete", c)
	}
	if c := costs["heavy"]; c == nil || c.Complete() || c.Unpriced[0] != "broker-hours" {
		t.Errorf("heavy: %+v, want broker-hours unpriced", c)
	}
}

func TestTheADRShowsCostsWithTheirDateAndMarksALowerBound(t *testing.T) {
	f := costedFamily{}
	d, _ := NewDecision("adr-c", "Costed", f, t0)
	_ = d.Answer(f, map[string]any{"rate": 5.0}, t0)
	_ = d.Assess(f, t0)
	_ = d.Decide(f, "simple", "cheap", t0)
	md := RenderADR(d, f)
	for _, want := range []string{
		"| Est. monthly cost |",
		"| $4.00 |",
		"at least $4.00",
		"published 2026-11-26",
		"Estimated monthly cost of Simple in us-east-1",
		"| Requests | 5,000,000 requests | $4.00 |",
		"- Assumes: no batching",
		"<https://pricing.example/offers/>",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("ADR lacks %q:\n%s", want, md)
		}
	}
}

func TestAFamilyWithoutACostModelGetsNoCostSection(t *testing.T) {
	d, f := newFake(t)
	_ = d.Assess(f, t0)
	md := RenderADR(d, f)
	if strings.Contains(md, "monthly cost") {
		t.Errorf("unexpected cost output:\n%s", md)
	}
}

func TestDollarsPrintToTheCentWithSeparators(t *testing.T) {
	for in, want := range map[float64]string{0: "$0.00", 0.004: "$0.00", 31.536: "$31.54", 1234567.891: "$1,234,567.89"} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %q, want %q", in, got, want)
		}
	}
}
