package app

import (
	"testing"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/advise/families/messaging"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/loadrun"
	"github.com/stratus/internal/pricing"
)

func TestTheEmbeddedSnapshotPricesEveryMessagingItem(t *testing.T) {
	book := snapshotPrices(pricing.Default())
	for _, region := range []string{"us-east-1", "eu-west-1", "ap-southeast-2"} {
		for _, id := range messaging.PriceIDs() {
			p, ok := book.Price(id, region)
			if !ok {
				t.Errorf("no %s price in %s", id, region)
				continue
			}
			if p.AsOf != pricing.Default().Published || p.Source == "" || len(p.Tiers) == 0 {
				t.Errorf("%s in %s is undated or empty: %+v", id, region, p)
			}
		}
	}
}

func TestAPriceMissingFromTheSnapshotIsNotFound(t *testing.T) {
	book := snapshotPrices(pricing.Default())
	if _, ok := book.Price("no-such-price", "us-east-1"); ok {
		t.Error("found an unknown price")
	}
	if _, ok := book.Price("sqs-standard-requests", "mars-north-1"); ok {
		t.Error("found a price in an unknown region")
	}
}

func TestTheMessagingFamilyIsWiredWithQuotasAndPrices(t *testing.T) {
	f := messaging.New(limitsQuotas, snapshotPrices(pricing.Default()))
	for _, e := range f.Costs(advise.NewRequirements(f.Questions(), nil)) {
		if !e.Complete() || e.MonthlyUSD <= 0 {
			t.Errorf("%s: %+v", e.Option, e)
		}
	}
}

func TestALoadRunBecomesAProofsRunResult(t *testing.T) {
	s := loadrun.Summary{
		ID: "run-3", Status: loadrun.StatusFinished, DurationSeconds: 10,
		Totals:   loadrun.Totals{Counts: loadrun.Counts{Requests: 100, OK: 80, Throttled: 20, P99MS: 4}, PerSecond: 10},
		Limits:   &limits.Status{Rules: []limits.RuleStatus{{Rule: limits.RateRule{ID: "sns-publish-rate"}, Throttled: 20}, {Rule: limits.RateRule{ID: "kms"}}}},
		Findings: []loadrun.Finding{{Severity: loadrun.Severity("high"), Title: "No retries"}},
	}
	r := runResultOf(s)
	if !r.Finished || r.Throttled != 20 || r.P99MS != 4 || len(r.ThrottledBy) != 1 || r.ThrottledBy[0] != "sns-publish-rate" || r.Findings[0] != "high: No retries" {
		t.Errorf("got %+v", r)
	}
	if _, err := (recordedRuns{}).Run("run-1"); err == nil {
		t.Error("no recorder should be an error")
	}
}

func TestProofsOnlyUseOperationsStratusImplements(t *testing.T) {
	if !implementedOperations.Implemented("sns", "Publish") || implementedOperations.Implemented("sns", "NoSuchOperation") {
		t.Error("coverage adapter disagrees with internal/awsmodel")
	}
}
