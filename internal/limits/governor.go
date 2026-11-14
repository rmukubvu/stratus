package limits

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratus/internal/control"
)

// BreachSink receives every breach as it happens. Sinks run on the request
// path, so they must be quick and must not call back into the governor.
type BreachSink func(Breach)

const (
	breachLogSize = 500
	// maxBuckets bounds memory when many distinct resources or prefixes
	// are seen; past it, buckets that have refilled are forgotten, which
	// is safe because a new bucket starts full anyway.
	maxBuckets = 50000
)

type ruleStats struct {
	admitted  int
	throttled int
}

// state is one applied profile and everything it has counted.
type state struct {
	profile    Profile
	appliedAt  time.Time
	buckets    map[QuotaKey]*tokenBucket
	semaphores map[QuotaKey]*semaphore
	stats      []ruleStats
	// concurrency counts admitted and throttled acquisitions per rule ID.
	concurrency map[string]*ruleStats
	// capacity counts DynamoDB units per account/region/table.
	capacity map[string]*capacityStats
	breaches []Breach
}

// Governor applies a limits profile to traffic. At most one profile is
// active at a time. It is safe for concurrent use.
type Governor struct {
	known map[string]bool
	clock Clock

	// active is nil while limits are off, so the request path pays one
	// atomic load and nothing else.
	active atomic.Pointer[state]

	mu    sync.Mutex
	sinks []BreachSink
	// emulator reports how loaded Stratus itself is, so a status can tell
	// AWS's limits apart from the emulator's own.
	emulator func() any
	// lastBreaches keeps the breaches of the profile most recently
	// cleared, so failures can still be explained after it ends.
	lastBreaches []Breach
}

// NewGovernor builds a governor. knownServices validates rule targets; nil
// accepts any service name. A nil clock uses the system clock.
func NewGovernor(knownServices []string, clock Clock) *Governor {
	var known map[string]bool
	if knownServices != nil {
		known = make(map[string]bool, len(knownServices))
		for _, s := range knownServices {
			known[s] = true
		}
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Governor{known: known, clock: clock}
}

// SetEmulatorStats installs a report of Stratus's own load (such as its
// Lambda containers) that Status includes.
func (g *Governor) SetEmulatorStats(stats func() any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.emulator = stats
}

func (g *Governor) withEmulator(s Status) Status {
	if g.emulator != nil {
		s.Emulator = g.emulator()
	}
	return s
}

// Subscribe adds a sink that receives every breach from now on.
func (g *Governor) Subscribe(sink BreachSink) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sinks = append(g.sinks, sink)
}

// Apply resolves a spec and makes it the active profile, replacing any
// current one and starting its counters from zero.
func (g *Governor) Apply(spec Spec) (Status, error) {
	profile, err := Resolve(spec, g.known)
	if err != nil {
		return Status{}, err
	}
	st := &state{
		profile:     profile,
		appliedAt:   g.clock.Now(),
		buckets:     map[QuotaKey]*tokenBucket{},
		semaphores:  map[QuotaKey]*semaphore{},
		stats:       make([]ruleStats, len(profile.Rates)),
		concurrency: map[string]*ruleStats{},
		capacity:    map[string]*capacityStats{},
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active.Store(st)
	return g.withEmulator(st.status()), nil
}

// Clear turns limits off and returns the final status.
func (g *Governor) Clear() (Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.active.Load()
	if st == nil {
		return Status{}, noProfile()
	}
	status := g.withEmulator(st.status())
	g.lastBreaches = st.breaches
	g.active.Store(nil)
	return status, nil
}

// Status describes the active profile and its counters.
func (g *Governor) Status() (Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.active.Load()
	if st == nil {
		return Status{}, noProfile()
	}
	return g.withEmulator(st.status()), nil
}

// Enabled reports whether a profile is active.
func (g *Governor) Enabled() bool { return g.active.Load() != nil }

// Breaches returns the recorded breaches of the active profile, or of the
// last cleared one when none is active, oldest first.
func (g *Governor) Breaches() []Breach {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.active.Load(); st != nil {
		return append([]Breach{}, st.breaches...)
	}
	return append([]Breach{}, g.lastBreaches...)
}

// Admit checks a request against every rate rule that covers it. It
// returns nil and counts the request against each rule's bucket, or
// returns the first breach and counts nothing, as a throttled request does
// not consume capacity.
func (g *Governor) Admit(req Request) *Breach {
	if g.active.Load() == nil {
		return nil
	}
	g.mu.Lock()
	st := g.active.Load()
	if st == nil {
		g.mu.Unlock()
		return nil
	}
	now := g.clock.Now()

	type hit struct {
		bucket *tokenBucket
		rule   int
	}
	var hits []hit
	for i, r := range st.profile.Rates {
		if !r.matches(req) {
			continue
		}
		key := r.keyFor(req)
		b := st.bucket(key, st.profile.EffectiveRate(r), st.profile.capacity(r), now)
		if !b.has(now, 1) {
			st.stats[i].throttled++
			breach := Breach{
				Time: now.UTC(), RuleID: r.ID, Kind: KindRate, Key: key.String(),
				Limit: st.profile.EffectiveRate(r), RequestID: req.RequestID,
				Service: req.Service, Operation: req.Operation, Resource: firstNonEmpty(req.Prefix, req.Resource),
			}
			st.record(breach)
			sinks := g.sinks
			g.mu.Unlock()
			notify(sinks, breach)
			return &breach
		}
		hits = append(hits, hit{bucket: b, rule: i})
	}
	for _, h := range hits {
		h.bucket.take(1)
		st.stats[h.rule].admitted++
	}
	g.mu.Unlock()
	return nil
}

// Acquire takes one unit of a concurrency quota for the request: up to max
// may be held under key at once. It returns a release function, or a
// breach when the quota is full. With no profile active it always
// succeeds. Release is idempotent.
func (g *Governor) Acquire(key QuotaKey, max int, req Request) (release func(), breach *Breach) {
	return g.acquire(key, max, req, "")
}

func (g *Governor) acquire(key QuotaKey, max int, req Request, reason string) (release func(), breach *Breach) {
	noop := func() {}
	if g.active.Load() == nil {
		return noop, nil
	}
	g.mu.Lock()
	st := g.active.Load()
	if st == nil {
		g.mu.Unlock()
		return noop, nil
	}
	stats := st.concurrency[key.RuleID]
	if stats == nil {
		stats = &ruleStats{}
		st.concurrency[key.RuleID] = stats
	}
	sem := st.semaphores[key]
	if sem == nil {
		sem = &semaphore{max: max}
		st.semaphores[key] = sem
	}
	sem.max = max // a profile change mid-flight takes effect at once
	if !sem.tryAcquire() {
		stats.throttled++
		b := Breach{
			Time: g.clock.Now().UTC(), RuleID: key.RuleID, Kind: KindConcurrency, Key: key.String(),
			Limit: float64(max), RequestID: req.RequestID,
			Service: req.Service, Operation: req.Operation, Resource: req.Resource, Reason: reason,
		}
		st.record(b)
		sinks := g.sinks
		g.mu.Unlock()
		notify(sinks, b)
		return noop, &b
	}
	stats.admitted++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			sem.release()
			g.mu.Unlock()
		})
	}, nil
}

func (st *state) bucket(key QuotaKey, rate, capacity float64, now time.Time) *tokenBucket {
	if b, ok := st.buckets[key]; ok {
		return b
	}
	if len(st.buckets) >= maxBuckets {
		st.pruneFullBuckets(now)
	}
	b := newTokenBucket(rate, capacity, now)
	st.buckets[key] = b
	return b
}

func (st *state) pruneFullBuckets(now time.Time) {
	for k, b := range st.buckets {
		if b.has(now, b.capacity) {
			delete(st.buckets, k)
		}
	}
}

func (st *state) record(b Breach) {
	if len(st.breaches) == breachLogSize {
		copy(st.breaches, st.breaches[1:])
		st.breaches = st.breaches[:breachLogSize-1]
	}
	st.breaches = append(st.breaches, b)
}

func notify(sinks []BreachSink, b Breach) {
	for _, sink := range sinks {
		sink(b)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func noProfile() *control.Error {
	return &control.Error{
		Kind:    control.KindNotFound,
		Code:    "NoLimitsProfile",
		Message: "no limits profile is active",
		Hint:    "apply one with PUT /_stratus/control/v1/limits",
	}
}
