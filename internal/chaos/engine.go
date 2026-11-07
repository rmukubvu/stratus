package chaos

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"

	"github.com/stratus/internal/control"
)

// Decision is what to do to a request. A zero Decision means "no fault".
type Decision struct {
	// Delay to apply before handling (or failing) the request.
	Delay time.Duration
	// ColdStart names a Lambda function whose warm container to discard.
	ColdStart string
	// Error, when set, is returned instead of handling the request.
	Error *ErrorSpec
	// Drop closes the connection without a response, or loses an event.
	Drop bool
	// FunctionError, when set, is the message of a Lambda function error
	// to return instead of invoking the function.
	FunctionError string
	// Duplicate delivers an event twice.
	Duplicate bool
	// RuleIDs lists the rules that fired, in order.
	RuleIDs []string
}

// Faulted reports whether any rule fired.
func (d Decision) Faulted() bool { return len(d.RuleIDs) > 0 }

// Hit records one injected fault, for diagnosis.
type Hit struct {
	Time   time.Time `json:"time"`
	RuleID string    `json:"rule_id"`
	Action Action    `json:"action"`
	// Scope is "api" for an API request or "event" for a delivery.
	Scope Scope `json:"scope"`
	// RequestID is the API request ID, or the delivery's event ID.
	RequestID string `json:"request_id"`
	Service   string `json:"service"`
	Operation string `json:"operation,omitempty"`
	// Path is the request path, or the delivery's target ARN.
	Path string `json:"path"`
	// Source and SourceARN say where a delivery came from.
	Source    string `json:"source,omitempty"`
	SourceARN string `json:"source_arn,omitempty"`
	// Match is the rule's 1-based count of matching requests at this hit.
	// Replaying with the same seed faults the same match numbers.
	Match int `json:"match"`
}

// RuleStats counts what a rule saw and did.
type RuleStats struct {
	Rule     Rule `json:"rule"`
	Matched  int  `json:"matched"`
	Injected int  `json:"injected"`
}

// Status is the state of an experiment.
type Status string

const (
	StatusRunning Status = "running"
	StatusExpired Status = "expired"
	StatusStopped Status = "stopped"
)

// Report describes an experiment and its effect so far.
type Report struct {
	// ID identifies this run of the experiment.
	ID              string      `json:"id"`
	Status          Status      `json:"status"`
	Seed            int64       `json:"seed"`
	StartedAt       time.Time   `json:"started_at"`
	EndsAt          *time.Time  `json:"ends_at,omitempty"`
	DurationSeconds int         `json:"duration_seconds,omitempty"`
	Rules           []RuleStats `json:"rules"`
	TotalInjected   int         `json:"total_injected"`
	// RecentHits holds the latest hits, oldest first.
	RecentHits []Hit `json:"recent_hits"`
}

// Clock lets tests control time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

const hitLogSize = 500

type ruleState struct {
	rule     Rule
	matched  int
	injected int
}

type experiment struct {
	id        string
	seed      int64
	startedAt time.Time
	endsAt    time.Time // zero when open-ended
	duration  int
	rules     []*ruleState
	hits      []Hit
	stopped   bool
}

// Engine holds at most one experiment at a time and decides, per request,
// which faults to inject. It is safe for concurrent use.
type Engine struct {
	mu    sync.Mutex
	exp   *experiment
	known map[string]bool
	clock Clock
	// lastHits keeps the hits of the most recently stopped experiment, so
	// failures can still be attributed to chaos after it ends.
	lastHits []Hit
}

// NewEngine builds an engine. knownServices validates rule targets; nil
// accepts any service name.
func NewEngine(knownServices []string, clock Clock) *Engine {
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
	return &Engine{known: known, clock: clock}
}

// Validate checks a spec the way Start would, without starting it.
func (e *Engine) Validate(spec Spec) error {
	_, err := spec.normalise(e.known)
	return err
}

// Start begins an experiment, replacing any current one.
func (e *Engine) Start(spec Spec) (Report, error) {
	spec, err := spec.normalise(e.known)
	if err != nil {
		return Report{}, err
	}
	if spec.Seed == 0 {
		spec.Seed = randomSeed()
	}
	now := e.clock.Now()
	exp := &experiment{id: newExperimentID(), seed: spec.Seed, startedAt: now, duration: spec.DurationSeconds}
	if spec.DurationSeconds > 0 {
		exp.endsAt = now.Add(time.Duration(spec.DurationSeconds) * time.Second)
	}
	for _, r := range spec.Rules {
		exp.rules = append(exp.rules, &ruleState{rule: r})
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.exp = exp
	return e.reportLocked(), nil
}

// Stop ends the current experiment and returns its final report.
func (e *Engine) Stop() (Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exp == nil {
		return Report{}, noExperiment()
	}
	return e.stopLocked(), nil
}

// StopID ends the current experiment only if it is the one with the given
// ID, so a caller cannot stop an experiment that replaced its own.
func (e *Engine) StopID(id string) (Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exp == nil || e.exp.id != id {
		return Report{}, noExperiment()
	}
	return e.stopLocked(), nil
}

func (e *Engine) stopLocked() Report {
	e.exp.stopped = true
	report := e.reportLocked()
	e.lastHits = e.exp.hits
	e.exp = nil
	return report
}

// Report describes the current experiment.
func (e *Engine) Report() (Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exp == nil {
		return Report{}, noExperiment()
	}
	return e.reportLocked(), nil
}

// Hits returns the recorded hits of the running experiment, or of the last
// stopped one when none is running.
func (e *Engine) Hits() []Hit {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exp != nil {
		return append([]Hit{}, e.exp.hits...)
	}
	return append([]Hit{}, e.lastHits...)
}

// Decide returns the faults to inject into an API request and records them.
//
// Rules are evaluated in order. Latency and cold starts accumulate; the
// first error or drop wins and later rules are not evaluated, so they do
// not count the request as a match.
func (e *Engine) Decide(req Request) Decision {
	return e.decide(req.traffic())
}

// DecideDelivery returns the faults to inject into an event delivery and
// records them. Rules are evaluated as for Decide; duplicates accumulate.
func (e *Engine) DecideDelivery(d Delivery) Decision {
	return e.decide(d.traffic())
}

func (e *Engine) decide(t traffic) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	exp := e.exp
	if exp == nil {
		return Decision{}
	}
	now := e.clock.Now()
	if !exp.endsAt.IsZero() && !now.Before(exp.endsAt) {
		return Decision{}
	}

	var d Decision
	for i, st := range exp.rules {
		r := st.rule
		if !r.matches(t) || !r.activeAt(exp.startedAt, now) {
			continue
		}
		st.matched++
		if r.MaxHits > 0 && st.injected >= r.MaxHits {
			continue
		}
		if !fires(exp.seed, i, st.matched, r.Probability) {
			continue
		}
		st.injected++
		d.RuleIDs = append(d.RuleIDs, r.ID)
		exp.recordLocked(Hit{
			Time: now.UTC(), RuleID: r.ID, Action: r.Action, Scope: t.scope, RequestID: t.id,
			Service: t.service, Operation: t.operation, Path: t.path,
			Source: t.source, SourceARN: t.sourceARN, Match: st.matched,
		})

		switch r.Action {
		case ActionLatency:
			d.Delay += latencyFor(exp.seed, i, st.matched, *r.Latency)
		case ActionColdStart:
			d.ColdStart = t.function
		case ActionDuplicate:
			d.Duplicate = true
		case ActionFunctionError:
			d.FunctionError = "Injected function error (chaos rule " + r.ID + ")"
			return d
		case ActionDrop:
			d.Drop = true
			return d
		default:
			d.Error = r.errorFor(t.service)
			return d
		}
	}
	return d
}

func (x *experiment) recordLocked(h Hit) {
	if len(x.hits) == hitLogSize {
		copy(x.hits, x.hits[1:])
		x.hits = x.hits[:hitLogSize-1]
	}
	x.hits = append(x.hits, h)
}

func (e *Engine) reportLocked() Report {
	x := e.exp
	status := StatusRunning
	switch {
	case x.stopped:
		status = StatusStopped
	case !x.endsAt.IsZero() && !e.clock.Now().Before(x.endsAt):
		status = StatusExpired
	}
	r := Report{
		ID:              x.id,
		Status:          status,
		Seed:            x.seed,
		StartedAt:       x.startedAt.UTC(),
		DurationSeconds: x.duration,
		RecentHits:      append([]Hit{}, x.hits...),
	}
	if !x.endsAt.IsZero() {
		ends := x.endsAt.UTC()
		r.EndsAt = &ends
	}
	for _, st := range x.rules {
		r.Rules = append(r.Rules, RuleStats{Rule: st.rule, Matched: st.matched, Injected: st.injected})
		r.TotalInjected += st.injected
	}
	return r
}

func noExperiment() *control.Error {
	return &control.Error{
		Kind:    control.KindNotFound,
		Code:    "NoChaosExperiment",
		Message: "no chaos experiment is running",
		Hint:    "start one with PUT /_stratus/control/v1/chaos",
	}
}

func newExperimentID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "chaos-" + hex.EncodeToString(b[:])
}

func randomSeed() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	seed := int64(binary.LittleEndian.Uint64(b[:]) &^ (1 << 63))
	if seed == 0 {
		seed = 1
	}
	return seed
}
