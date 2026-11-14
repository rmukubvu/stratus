package limits

import (
	"sort"
	"time"
)

// RuleStatus is one rate rule and what it has counted.
type RuleStatus struct {
	Rule RateRule `json:"rule"`
	// EffectivePerSecond is the rule's rate after scaling.
	EffectivePerSecond float64 `json:"effective_per_second"`
	Admitted           int     `json:"admitted"`
	Throttled          int     `json:"throttled"`
}

// ConcurrencyStatus counts acquisitions of one concurrency quota.
type ConcurrencyStatus struct {
	RuleID    string `json:"rule_id"`
	Admitted  int    `json:"admitted"`
	Throttled int    `json:"throttled"`
}

// Status describes the active profile and its effect so far.
type Status struct {
	Profile     string              `json:"profile"`
	Scale       float64             `json:"scale"`
	AppliedAt   time.Time           `json:"applied_at"`
	Rules       []RuleStatus        `json:"rules"`
	Concurrency []ConcurrencyStatus `json:"concurrency,omitempty"`
	// TotalThrottled counts breaches of every kind.
	TotalThrottled int `json:"total_throttled"`
	// RecentBreaches holds the latest breaches, oldest first.
	RecentBreaches []Breach `json:"recent_breaches"`
}

func (st *state) status() Status {
	s := Status{
		Profile:        st.profile.Name,
		Scale:          st.profile.Scale,
		AppliedAt:      st.appliedAt.UTC(),
		RecentBreaches: append([]Breach{}, st.breaches...),
	}
	for i, r := range st.profile.Rates {
		stats := st.stats[i]
		s.Rules = append(s.Rules, RuleStatus{
			Rule: r, EffectivePerSecond: st.profile.EffectiveRate(r),
			Admitted: stats.admitted, Throttled: stats.throttled,
		})
		s.TotalThrottled += stats.throttled
	}
	for id, stats := range st.concurrency {
		s.Concurrency = append(s.Concurrency, ConcurrencyStatus{RuleID: id, Admitted: stats.admitted, Throttled: stats.throttled})
		s.TotalThrottled += stats.throttled
	}
	sort.Slice(s.Concurrency, func(i, j int) bool { return s.Concurrency[i].RuleID < s.Concurrency[j].RuleID })
	return s
}
