package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/stratus/internal/loadrun"
)

// reportOperations is how many operations the text report lists.
const reportOperations = 10

// writeRunReport prints a run summary for a person: the totals, the
// busiest operations and what the limits and chaos did. The full summary,
// with its per-second timeline, stays available as JSON.
func writeRunReport(w io.Writer, raw json.RawMessage) error {
	var s loadrun.Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("reading run summary: %w", err)
	}
	title := s.ID
	if s.Name != "" {
		title += fmt.Sprintf(" (%s)", s.Name)
	}
	fmt.Fprintf(w, "%s %s after %.1fs\n\n", title, s.Status, s.DurationSeconds)

	t := s.Totals
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "requests\t%d\t%.1f/s\n", t.Requests, t.PerSecond)
	fmt.Fprintf(tw, "ok\t%d\t\n", t.OK)
	fmt.Fprintf(tw, "throttled\t%d\t%s\n", t.Throttled, percent(t.ThrottleRate))
	fmt.Fprintf(tw, "faults\t%d\tinjected by chaos\n", t.Faults)
	fmt.Fprintf(tw, "errors\t%d\t%s\n", t.Errors, errorsNote(t))
	_ = tw.Flush()
	fmt.Fprintf(w, "latency    p50 %s  p95 %s  p99 %s  max %s\n", ms(t.P50MS), ms(t.P95MS), ms(t.P99MS), ms(t.MaxMS))

	if len(s.Operations) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "operation\trequests\tthrottled\tfaults\terrors\tp95")
		for i, op := range s.Operations {
			if i == reportOperations {
				fmt.Fprintf(tw, "... %d more\t\t\t\t\t\n", len(s.Operations)-i)
				break
			}
			fmt.Fprintf(tw, "%s %s\t%d\t%d\t%d\t%d\t%s\n", op.Service, op.Operation, op.Requests, op.Throttled, op.Faults, op.Errors, ms(op.P95MS))
		}
		_ = tw.Flush()
	}

	if len(s.Findings) > 0 {
		fmt.Fprintf(w, "\nfindings:\n")
		for _, f := range s.Findings {
			fmt.Fprintf(w, "  %-6s  %s\n", strings.ToUpper(string(f.Severity)), f.Title)
		}
		fmt.Fprintf(w, "  what to do: stratus load report %s\n", s.ID)
	}

	if s.Limits != nil || s.Chaos != nil {
		fmt.Fprintln(w)
	}
	if s.Limits != nil {
		fmt.Fprintf(w, "limits: %s profile, %d throttled\n", s.Limits.Profile, s.Limits.TotalThrottled)
	}
	if s.Chaos != nil {
		fmt.Fprintf(w, "chaos:  %s (seed %d), %d faults injected\n", s.Chaos.ID, s.Chaos.Seed, s.Chaos.TotalInjected)
	}
	fmt.Fprintf(w, "\nTimeline and full detail: stratus load status %s\n", s.ID)
	return nil
}

// writeMarkdownReport prints a run as a Markdown report, findings and
// what to do about them included.
func writeMarkdownReport(w io.Writer, raw json.RawMessage) error {
	var s loadrun.Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("reading run summary: %w", err)
	}
	_, err := io.WriteString(w, loadrun.Markdown(s))
	return err
}

func errorsNote(t loadrun.Totals) string {
	if t.Errors == 0 {
		return ""
	}
	return percent(t.ErrorRate) + "; run 'stratus diagnose' to see why"
}

func percent(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func ms(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".") + "ms"
}
