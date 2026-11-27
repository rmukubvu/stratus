// Package pricing holds AWS on-demand prices from a dated snapshot of
// AWS's public price list, embedded in the binary. Prices live here once,
// as quotas live in internal/limits, so every estimate Stratus makes reads
// the same numbers and says how old they are.
//
// The snapshot is rebuilt by scripts/pricing/snapshot.py.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

//go:embed snapshot.json
var snapshotJSON []byte

// Tier is one band of a tiered price: units from From up to To (no upper
// bound when To is 0) cost USD each. Pricing usage across tiers is the
// estimate's job (advise.Price.Cost); this package stores and checks them.
type Tier struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
	USD  float64 `json:"usd"`
}

// Price is one AWS price, per region.
type Price struct {
	ID          string            `json:"id"`
	Service     string            `json:"service"`
	Unit        string            `json:"unit"`
	Description string            `json:"description"`
	Regions     map[string][]Tier `json:"regions"`
}

// In returns the price's tiers in region.
func (p Price) In(region string) ([]Tier, bool) {
	t, ok := p.Regions[region]
	return t, ok && len(t) > 0
}

// Snapshot is a dated set of prices.
type Snapshot struct {
	// Published is the newest publication date (YYYY-MM-DD) of the offer
	// files the prices came from.
	Published string `json:"published"`
	// Fetched is the date the snapshot was built.
	Fetched string `json:"fetched"`
	// Source is the AWS price list the offer files came from.
	Source string           `json:"source"`
	Prices map[string]Price `json:"prices"`
}

// Lookup returns a price by ID.
func (s Snapshot) Lookup(id string) (Price, bool) {
	p, ok := s.Prices[id]
	return p, ok
}

// IDs returns the price IDs, sorted.
func (s Snapshot) IDs() []string {
	ids := make([]string, 0, len(s.Prices))
	for id := range s.Prices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Parse reads a snapshot and checks that every tier list is well formed:
// it starts at zero, has no gaps and ends unbounded.
func Parse(raw []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, fmt.Errorf("pricing: %w", err)
	}
	if s.Published == "" || s.Source == "" {
		return Snapshot{}, fmt.Errorf("pricing: snapshot has no publication date or source")
	}
	for id, p := range s.Prices {
		p.ID = id
		s.Prices[id] = p
		for region, tiers := range p.Regions {
			if err := checkTiers(tiers); err != nil {
				return Snapshot{}, fmt.Errorf("pricing: %s in %s: %w", id, region, err)
			}
		}
	}
	return s, nil
}

func checkTiers(tiers []Tier) error {
	if len(tiers) == 0 {
		return fmt.Errorf("no tiers")
	}
	next := 0.0
	for i, t := range tiers {
		if t.From != next || t.USD < 0 {
			return fmt.Errorf("tier %d starts at %v (want %v) or has a negative price", i, t.From, next)
		}
		last := i == len(tiers)-1
		if last != (t.To == 0) || (!last && t.To <= t.From) {
			return fmt.Errorf("tier %d ends at %v; only the last tier is unbounded", i, t.To)
		}
		next = t.To
	}
	return nil
}

var (
	defaultOnce     sync.Once
	defaultSnapshot Snapshot
)

// Default returns the snapshot embedded in the binary.
func Default() Snapshot {
	defaultOnce.Do(func() {
		s, err := Parse(snapshotJSON)
		if err != nil {
			panic(err) // the embedded file is checked by tests
		}
		defaultSnapshot = s
	})
	return defaultSnapshot
}
