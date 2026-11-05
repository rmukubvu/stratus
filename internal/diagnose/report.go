package diagnose

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Failure is one failed request, as recorded by the server.
type Failure struct {
	Time      time.Time
	RequestID string
	Service   string
	Operation string
	Method    string
	Path      string
	Status    int
	Code      string
	Message   string
}

// Injection says a chaos rule caused a request's failure.
type Injection struct {
	RuleID string
	Action string
}

// Group is a set of failures with the same service, operation and code.
type Group struct {
	Service    string    `json:"service"`
	Operation  string    `json:"operation,omitempty"`
	Code       string    `json:"code"`
	Status     int       `json:"status"`
	Count      int       `json:"count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Message    string    `json:"message"`
	RequestIDs []string  `json:"example_request_ids"`
	Paths      []string  `json:"example_paths"`
	Explanation
}

// Report is the result of a diagnosis.
type Report struct {
	// Summary is one sentence suitable for an agent's context window.
	Summary string `json:"summary"`
	// Healthy is true when nothing failed except injected faults.
	Healthy  bool             `json:"healthy"`
	Since    *time.Time       `json:"since,omitempty"`
	Failures int              `json:"failures"`
	ByCause  map[Category]int `json:"by_category"`
	Groups   []Group          `json:"groups"`
}

const maxExamples = 3

// Options narrows a diagnosis.
type Options struct {
	// Since drops failures before this time; zero keeps all.
	Since time.Time
	// Service keeps only one service; empty keeps all.
	Service string
	// ExcludeInjected drops failures caused by chaos rules.
	ExcludeInjected bool
	// MaxGroups caps the number of groups returned; zero means 20.
	MaxGroups int
}

type groupKey struct {
	service, operation, code, ruleID string
}

// Diagnose groups failures and explains each group. injected maps request
// IDs to the chaos rule that caused them.
func Diagnose(failures []Failure, injected map[string]Injection, opts Options) Report {
	groups := map[groupKey]*Group{}
	byCause := map[Category]int{}
	total := 0

	for _, f := range failures {
		if !opts.Since.IsZero() && f.Time.Before(opts.Since) {
			continue
		}
		if opts.Service != "" && f.Service != opts.Service {
			continue
		}
		inj, wasInjected := injected[f.RequestID]
		if wasInjected && opts.ExcludeInjected {
			continue
		}

		key := groupKey{service: f.Service, operation: f.Operation, code: f.Code}
		if wasInjected {
			key.ruleID = inj.RuleID
		}
		g, ok := groups[key]
		if !ok {
			var exp Explanation
			if wasInjected {
				exp = explainInjected(inj.RuleID, inj.Action)
			} else {
				exp = explain(failure{service: f.Service, operation: f.Operation, code: f.Code, message: f.Message, status: f.Status})
			}
			g = &Group{
				Service: f.Service, Operation: f.Operation, Code: f.Code, Status: f.Status,
				FirstSeen: f.Time, LastSeen: f.Time, Message: f.Message, Explanation: exp,
			}
			groups[key] = g
		}
		g.add(f)
		byCause[g.Category]++
		total++
	}

	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		// Real problems before injected faults, then most recent first.
		if (out[i].Category == CategoryInjected) != (out[j].Category == CategoryInjected) {
			return out[j].Category == CategoryInjected
		}
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Count > out[j].Count
	})
	limit := opts.MaxGroups
	if limit <= 0 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}

	report := Report{
		Healthy:  total == byCause[CategoryInjected],
		Failures: total,
		ByCause:  byCause,
		Groups:   out,
	}
	if !opts.Since.IsZero() {
		since := opts.Since.UTC()
		report.Since = &since
	}
	report.Summary = summarise(report)
	return report
}

func (g *Group) add(f Failure) {
	g.Count++
	if f.Time.Before(g.FirstSeen) {
		g.FirstSeen = f.Time
	}
	if !f.Time.Before(g.LastSeen) {
		g.LastSeen = f.Time
		g.Message = f.Message
	}
	if len(g.RequestIDs) < maxExamples && f.RequestID != "" {
		g.RequestIDs = append(g.RequestIDs, f.RequestID)
	}
	if len(g.Paths) < maxExamples && f.Path != "" && !contains(g.Paths, f.Path) {
		g.Paths = append(g.Paths, f.Path)
	}
}

var categoryOrder = []Category{
	CategoryEmulator, CategoryUnsupported, CategoryMissingResource, CategoryConflict,
	CategoryInvalidRequest, CategoryAuth, CategoryThrottling, CategoryConnection,
	CategoryClient, CategoryInjected,
}

var categoryLabel = map[Category]string{
	CategoryEmulator:        "emulator error",
	CategoryUnsupported:     "unsupported operation",
	CategoryMissingResource: "missing resource",
	CategoryConflict:        "conflict",
	CategoryInvalidRequest:  "invalid request",
	CategoryAuth:            "auth failure",
	CategoryThrottling:      "throttled",
	CategoryConnection:      "dropped connection",
	CategoryClient:          "other client error",
	CategoryInjected:        "injected by chaos",
}

func summarise(r Report) string {
	if r.Failures == 0 {
		return "No failed requests."
	}
	var parts []string
	for _, c := range categoryOrder {
		if n := r.ByCause[c]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, categoryLabel[c]))
		}
	}
	noun := "requests"
	if r.Failures == 1 {
		noun = "request"
	}
	s := fmt.Sprintf("%d failed %s in %d group(s): %s.", r.Failures, noun, len(r.Groups), strings.Join(parts, ", "))
	if r.Healthy {
		s += " All failures were injected by chaos rules."
	} else if len(r.Groups) > 0 {
		top := r.Groups[0]
		s += fmt.Sprintf(" Start with %s %s (%s): %s.", top.Service, top.Operation, top.Code, top.LikelyCause)
	}
	return s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
