package app

import (
	"testing"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/advise/families/messaging"
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
