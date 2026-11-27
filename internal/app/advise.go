package app

import (
	"time"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/advise/families/messaging"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/pricing"
	"github.com/stratus/internal/store"
)

// limitsQuotas backs advice with the quotas internal/limits documents, so
// a decision, an enforced limit and a load run read the same numbers.
var limitsQuotas = advise.QuotaFunc(func(id, region string) (advise.Quota, bool) {
	q, ok := limits.DocumentedQuota(id)
	if !ok {
		return advise.Quota{}, false
	}
	return advise.Quota{ID: q.ID, Value: q.In(region), Source: q.Source}, true
})

// snapshotPrices backs cost estimates with the dated AWS price snapshot
// embedded in the binary.
func snapshotPrices(s pricing.Snapshot) advise.PriceBook {
	return advise.PriceFunc(func(id, region string) (advise.Price, bool) {
		p, ok := s.Lookup(id)
		if !ok {
			return advise.Price{}, false
		}
		tiers, ok := p.In(region)
		if !ok {
			return advise.Price{}, false
		}
		out := advise.Price{ID: p.ID, Unit: p.Unit, AsOf: s.Published, Source: s.Source}
		for _, t := range tiers {
			out.Tiers = append(out.Tiers, advise.PriceTier{From: t.From, To: t.To, USD: t.USD})
		}
		return out, true
	})
}

// newAdviseService wires guided architecture decisions: the families
// Stratus advises on, stored in the default scope's metadata.
func newAdviseService(meta store.Store, now func() time.Time) *advise.Service {
	families, err := advise.NewRegistry(messaging.New(limitsQuotas, snapshotPrices(pricing.Default())))
	if err != nil {
		panic(err) // static data; covered by tests
	}
	return advise.NewService(advise.NewStoreRepository(meta), families, now, nil)
}
