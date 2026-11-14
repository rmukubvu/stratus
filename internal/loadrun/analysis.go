package loadrun

import (
	"fmt"
	"sort"
	"strings"
)

// Severity ranks a finding.
type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

func (s Severity) rank() int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	}
	return 0
}

// AtLeast reports whether s is as severe as min.
func (s Severity) AtLeast(min Severity) bool { return s.rank() >= min.rank() }

// ParseSeverity reads low, medium or high.
func ParseSeverity(v string) (Severity, error) {
	s := Severity(strings.ToLower(strings.TrimSpace(v)))
	if s.rank() == 0 {
		return "", fmt.Errorf("severity %q: want low, medium or high", v)
	}
	return s, nil
}

// Detector names.
const (
	DetectorHotPartition   = "hot_partition"
	DetectorChattyCalls    = "chatty_calls"
	DetectorCacheableReads = "cacheable_reads"
	DetectorMissingRetries = "missing_retries"
	DetectorRetryStorm     = "retry_storm"
	DetectorMissingQueue   = "missing_queue"
)

// Finding is a problem a detector spotted in a run's traffic, with what
// to do about it. Detectors are deterministic: the same traffic gives the
// same findings.
type Finding struct {
	Detector   string         `json:"detector"`
	Severity   Severity       `json:"severity"`
	Title      string         `json:"title"`
	Detail     string         `json:"detail"`
	Suggestion string         `json:"suggestion"`
	Service    string         `json:"service,omitempty"`
	Operation  string         `json:"operation,omitempty"`
	Resource   string         `json:"resource,omitempty"`
	Evidence   map[string]any `json:"evidence,omitempty"`
}

// detector watches a run's traffic and reports what it spotted. Each
// keeps bounded state, so a long run costs no more memory than a short
// one.
type detector interface {
	observe(o Observation, oc outcome, sec int)
	findings(f runFacts) []Finding
}

// runFacts is what detectors may read about the whole run.
type runFacts struct {
	seconds    float64
	total      *counters
	operations map[opKey]*counters
}

// rate is requests per second over the run.
func (f runFacts) rate(n int) float64 {
	if f.seconds <= 0 {
		return float64(n)
	}
	return float64(n) / f.seconds
}

func (f runFacts) count(service, operation string) int {
	if c := f.operations[opKey{service, operation}]; c != nil {
		return c.requests
	}
	return 0
}

func newDetectors() []detector {
	return []detector{newHotPartitions(), batchableCalls{}, cacheableReads{}, newRetries(), newQueues()}
}

// retryable is whether an AWS SDK would retry a request that ended so.
func retryable(o Observation, oc outcome) bool {
	return oc == outcomeThrottled || o.Status == 0 || o.Status >= 500
}

// sortFindings puts the most severe first, then groups by detector.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if a, b := fs[i].Severity.rank(), fs[j].Severity.rank(); a != b {
			return a > b
		}
		return fs[i].Detector < fs[j].Detector
	})
}

// Highest is the most severe finding's severity, or "" with none.
func Highest(fs []Finding) Severity {
	var top Severity
	for _, f := range fs {
		if f.Severity.rank() > top.rank() {
			top = f.Severity
		}
	}
	return top
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
