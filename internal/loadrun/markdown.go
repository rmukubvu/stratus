package loadrun

import (
	"fmt"
	"strings"
)

// markdownOperations is how many operations the Markdown report lists.
const markdownOperations = 15

// Markdown renders a run as a report a person can read or paste into a
// pull request: totals, findings with what to do, and the busiest
// operations.
func Markdown(s Summary) string {
	var b strings.Builder
	title := "Load run " + s.ID
	if s.Name != "" {
		title += " (" + s.Name + ")"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "%s after %.1fs, started %s.\n\n", s.Status, s.DurationSeconds, s.StartedAt.Format("2006-01-02 15:04:05 MST"))

	t := s.Totals
	b.WriteString("| Requests | Per second | OK | Throttled | Faults | Errors | p50 | p95 | p99 |\n")
	b.WriteString("| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	fmt.Fprintf(&b, "| %d | %.1f | %d | %d (%s) | %d | %d (%s) | %s | %s | %s |\n\n",
		t.Requests, t.PerSecond, t.OK, t.Throttled, pct(t.ThrottleRate), t.Faults, t.Errors, pct(t.ErrorRate),
		mdMS(t.P50MS), mdMS(t.P95MS), mdMS(t.P99MS))

	if s.Limits != nil || s.Chaos != nil {
		if s.Limits != nil {
			fmt.Fprintf(&b, "- Limits: `%s` profile, %d requests throttled.\n", s.Limits.Profile, s.Limits.TotalThrottled)
		}
		if s.Chaos != nil {
			fmt.Fprintf(&b, "- Chaos: experiment `%s` (seed %d), %d faults injected.\n", s.Chaos.ID, s.Chaos.Seed, s.Chaos.TotalInjected)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Findings\n\n")
	if len(s.Findings) == 0 {
		b.WriteString("No problems spotted in this run's traffic.\n\n")
	}
	for _, f := range s.Findings {
		fmt.Fprintf(&b, "### %s: %s\n\n%s\n\n**What to do:** %s\n\n", strings.ToUpper(string(f.Severity)), f.Title, f.Detail, f.Suggestion)
	}

	if len(s.Operations) > 0 {
		b.WriteString("## Operations\n\n")
		b.WriteString("| Operation | Requests | Throttled | Faults | Errors | p95 |\n")
		b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
		for i, op := range s.Operations {
			if i == markdownOperations {
				fmt.Fprintf(&b, "\n%d more operations not shown.\n", len(s.Operations)-i)
				break
			}
			fmt.Fprintf(&b, "| %s %s | %d | %d | %d | %d | %s |\n", op.Service, op.Operation, op.Requests, op.Throttled, op.Faults, op.Errors, mdMS(op.P95MS))
		}
	}
	return b.String()
}

func mdMS(v float64) string { return fmt.Sprintf("%gms", v) }
