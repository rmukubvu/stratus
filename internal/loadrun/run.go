// Package loadrun records what happens to AWS traffic during a load test.
// A run is a window, usually the length of one k6 test, during which
// Stratus counts every request per second and per operation: how many
// succeeded, were throttled, failed on purpose (chaos) or failed for real,
// and how long they took. A run can also apply a limits profile and start
// a chaos experiment for its duration, so one call sets up the whole test
// and one call ends it.
package loadrun

import (
	"sort"
	"time"
)

// Observation is one finished AWS request.
type Observation struct {
	Time      time.Time
	Service   string
	Operation string
	// Status is the HTTP status; zero when the connection was dropped.
	Status   int
	Code     string
	Duration time.Duration
	// Fault is true when a chaos rule failed or slowed the request.
	Fault bool
	// LimitRule names the limits rule that throttled the request, when
	// the HTTP layer throttled it.
	LimitRule string
	// InvocationID is shared by every attempt of one AWS SDK call, and
	// Attempt and MaxAttempts number them; empty for other clients.
	InvocationID         string
	Attempt, MaxAttempts int
	// InvocationType is a Lambda Invoke's invocation type.
	InvocationType string
	// Keys are the resources and partitions the request touched, as far
	// as the service noted them (DynamoDB tables and partition keys).
	Keys []Key
}

// Key is a resource and partition a request touched.
type Key struct {
	Resource  string
	Partition string
}

// throttleCodes are the error codes AWS uses for throttling, so throttles
// raised inside a service (DynamoDB capacity, Lambda concurrency) count
// as throttles too.
var throttleCodes = map[string]bool{
	"ThrottlingException": true, "Throttling": true, "SlowDown": true, "TooManyRequestsException": true,
	"ProvisionedThroughputExceededException": true, "RequestLimitExceeded": true, "RequestThrottled": true,
	"ThrottledException": true, "LimitExceededException": true,
}

// outcome is what became of a request.
type outcome int

const (
	outcomeOK outcome = iota
	outcomeThrottled
	outcomeFault
	outcomeError
)

func classify(o Observation) outcome {
	switch {
	case o.Status > 0 && o.Status < 400:
		return outcomeOK
	case o.LimitRule != "" || throttleCodes[o.Code]:
		return outcomeThrottled
	case o.Fault:
		return outcomeFault
	}
	return outcomeError
}

// counters count requests by outcome, with their latency.
type counters struct {
	requests, ok, throttled, faults, errors int
	latency                                 histogram
}

func (c *counters) add(o Observation, oc outcome) {
	c.requests++
	switch oc {
	case outcomeOK:
		c.ok++
	case outcomeThrottled:
		c.throttled++
	case outcomeFault:
		c.faults++
	default:
		c.errors++
	}
	c.latency.add(o.Duration)
}

// Counts are a report's view of counters.
type Counts struct {
	Requests  int `json:"requests"`
	OK        int `json:"ok"`
	Throttled int `json:"throttled"`
	// Faults failed because a chaos rule made them fail.
	Faults int `json:"faults"`
	// Errors failed for any other reason: the ones to look at first.
	Errors int     `json:"errors"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

func (c *counters) counts() Counts {
	return Counts{
		Requests: c.requests, OK: c.ok, Throttled: c.throttled, Faults: c.faults, Errors: c.errors,
		P50MS: c.latency.percentile(0.50), P95MS: c.latency.percentile(0.95),
		P99MS: c.latency.percentile(0.99), MaxMS: round(c.latency.max),
	}
}

// Second is one second of the timeline.
type Second struct {
	// T is the second's offset from the start of the run.
	T int `json:"t"`
	Counts
}

// OperationStats counts one service operation over the run.
type OperationStats struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
	Counts
}

// Totals counts the whole run.
type Totals struct {
	Counts
	// PerSecond is the mean request rate.
	PerSecond float64 `json:"per_second"`
	// ThrottleRate and ErrorRate are fractions of all requests.
	ThrottleRate float64 `json:"throttle_rate"`
	ErrorRate    float64 `json:"error_rate"`
}

// maxTimeline caps the timeline at an hour of seconds; totals and
// operations keep counting after that.
const maxTimeline = 3600

// maxOperations caps how many operations a report lists, busiest first.
const maxOperations = 50

type opKey struct{ service, operation string }

// tally aggregates one run's observations.
type tally struct {
	start      time.Time
	last       time.Time
	total      counters
	timeline   []*counters
	operations map[opKey]*counters
	detectors  []detector
}

func newTally(start time.Time) *tally {
	return &tally{start: start, operations: map[opKey]*counters{}, detectors: newDetectors()}
}

func (t *tally) add(o Observation) {
	if o.Time.Before(t.start) {
		return
	}
	oc := classify(o)
	t.total.add(o, oc)
	if o.Time.After(t.last) {
		t.last = o.Time
	}
	sec := int(o.Time.Sub(t.start) / time.Second)
	for _, d := range t.detectors {
		d.observe(o, oc, sec)
	}
	if sec < maxTimeline {
		for len(t.timeline) <= sec {
			t.timeline = append(t.timeline, nil)
		}
		if t.timeline[sec] == nil {
			t.timeline[sec] = &counters{}
		}
		t.timeline[sec].add(o, oc)
	}
	k := opKey{o.Service, o.Operation}
	c := t.operations[k]
	if c == nil {
		c = &counters{}
		t.operations[k] = c
	}
	c.add(o, oc)
}

// findings runs every detector over the run so far, most severe first.
func (t *tally) findings(end time.Time) []Finding {
	facts := runFacts{seconds: end.Sub(t.start).Seconds(), total: &t.total, operations: t.operations}
	out := []Finding{}
	for _, d := range t.detectors {
		out = append(out, d.findings(facts)...)
	}
	sortFindings(out)
	return out
}

func (t *tally) totals(end time.Time) Totals {
	tot := Totals{Counts: t.total.counts()}
	if secs := end.Sub(t.start).Seconds(); secs > 0 {
		tot.PerSecond = round(float64(t.total.requests) / secs)
	}
	if t.total.requests > 0 {
		tot.ThrottleRate = round(float64(t.total.throttled) / float64(t.total.requests))
		tot.ErrorRate = round(float64(t.total.errors) / float64(t.total.requests))
	}
	return tot
}

// seconds returns the timeline with empty seconds filled in, so a gap in
// traffic shows as zeros rather than disappearing.
func (t *tally) seconds() []Second {
	out := make([]Second, len(t.timeline))
	for i, c := range t.timeline {
		out[i] = Second{T: i}
		if c != nil {
			out[i].Counts = c.counts()
		}
	}
	return out
}

func (t *tally) operationStats() []OperationStats {
	out := make([]OperationStats, 0, len(t.operations))
	for k, c := range t.operations {
		out = append(out, OperationStats{Service: k.service, Operation: k.operation, Counts: c.counts()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Service+out[i].Operation < out[j].Service+out[j].Operation
	})
	if len(out) > maxOperations {
		out = out[:maxOperations]
	}
	return out
}
