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

// CapacityStatus counts the units one DynamoDB table consumed and the
// requests it throttled.
type CapacityStatus struct {
	Table          string  `json:"table"`
	Account        string  `json:"account"`
	Region         string  `json:"region"`
	Partitions     int     `json:"partitions"`
	ReadUnits      float64 `json:"read_units"`
	WriteUnits     float64 `json:"write_units"`
	ReadThrottled  int     `json:"read_throttled"`
	WriteThrottled int     `json:"write_throttled"`
}

// Status describes the active profile and its effect so far.
type Status struct {
	Profile     string              `json:"profile"`
	Scale       float64             `json:"scale"`
	AppliedAt   time.Time           `json:"applied_at"`
	Rules       []RuleStatus        `json:"rules"`
	Concurrency []ConcurrencyStatus `json:"concurrency,omitempty"`
	// DynamoDB is the table capacity model, when it is on.
	DynamoDB *DynamoDBSpec `json:"dynamodb,omitempty"`
	// Capacity counts each DynamoDB table that has seen traffic.
	Capacity []CapacityStatus `json:"capacity,omitempty"`
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
	s.DynamoDB = st.profile.DynamoDB
	for _, c := range st.capacity {
		s.Capacity = append(s.Capacity, CapacityStatus{
			Table: c.table, Account: c.account, Region: c.region, Partitions: c.partitions,
			ReadUnits: c.readUnits, WriteUnits: c.writeUnits, ReadThrottled: c.readThrottled, WriteThrottled: c.writeThrottled,
		})
		s.TotalThrottled += c.readThrottled + c.writeThrottled
	}
	sort.Slice(s.Capacity, func(i, j int) bool {
		a, b := s.Capacity[i], s.Capacity[j]
		return a.Account+a.Region+a.Table < b.Account+b.Region+b.Table
	})
	return s
}
