package app

import (
	"time"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/advise/families/messaging"
	"github.com/stratus/internal/awsmodel"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/loadrun"
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
func newAdviseService(meta store.Store, now func() time.Time, runs *loadrun.Recorder) *advise.Service {
	families, err := advise.NewRegistry(messaging.New(limitsQuotas, snapshotPrices(pricing.Default())))
	if err != nil {
		panic(err) // static data; covered by tests
	}
	svc := advise.NewService(advise.NewStoreRepository(meta), families, now, nil)
	svc.SetProofs(implementedOperations, recordedRuns{runs})
	return svc
}

// implementedOperations backs proof plans with Stratus's measured
// operation coverage, so a proof never leans on a 501.
var implementedOperations = advise.CoverageFunc(func(service, operation string) bool {
	return awsmodel.Lookup(service, operation) == awsmodel.Implemented
})

// recordedRuns reads load runs for proofs.
type recordedRuns struct{ rec *loadrun.Recorder }

func (r recordedRuns) Run(id string) (advise.RunResult, error) {
	if r.rec == nil {
		return advise.RunResult{}, errNoRecorder
	}
	s, err := r.rec.Get(id)
	if err != nil {
		return advise.RunResult{}, err
	}
	return runResultOf(s), nil
}

var errNoRecorder = &control.Error{Kind: control.KindConflict, Code: "LoadRunsUnavailable", Message: "load runs are not recorded here", Hint: "use a Stratus started by 'stratus serve'"}

func runResultOf(s loadrun.Summary) advise.RunResult {
	out := advise.RunResult{
		ID: s.ID, Finished: s.Status == loadrun.StatusFinished, DurationSeconds: s.DurationSeconds,
		Requests: s.Totals.Requests, OK: s.Totals.OK, Throttled: s.Totals.Throttled, Faults: s.Totals.Faults,
		Errors: s.Totals.Errors, PerSecond: s.Totals.PerSecond, P99MS: s.Totals.P99MS,
	}
	if s.Limits != nil {
		for _, r := range s.Limits.Rules {
			if r.Throttled > 0 {
				out.ThrottledBy = append(out.ThrottledBy, r.Rule.ID)
			}
		}
	}
	for _, f := range s.Findings {
		out.Findings = append(out.Findings, string(f.Severity)+": "+f.Title)
	}
	return out
}
