package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// proofPlan is a control-API proof plan, as the CLI reads it.
type proofPlan struct {
	Option          string  `json:"option"`
	Provable        bool    `json:"provable"`
	Reason          string  `json:"reason"`
	Account         string  `json:"account"`
	Rate            float64 `json:"rate"`
	DurationSeconds int     `json:"duration_seconds"`
	Scale           float64 `json:"scale"`
	Limits          struct {
		Profile string  `json:"profile"`
		Scale   float64 `json:"scale"`
	} `json:"limits"`
	Script string `json:"script"`
}

type proveArgs struct {
	id       string
	options  []string
	scale    float64
	duration time.Duration
	k6       string
	scripts  string
	dryRun   bool
}

func parseProve(args []string, stderr io.Writer) (proveArgs, error) {
	fs := flag.NewFlagSet("stratus advise prove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p proveArgs
	fs.Func("option", "Prove only this option (repeatable); default: every option that was not ruled out", func(v string) error {
		p.options = append(p.options, v)
		return nil
	})
	fs.Float64Var(&p.scale, "scale", 1, "Run at this fraction of the peak, with every quota scaled the same, e.g. 0.1")
	fs.DurationVar(&p.duration, "duration", 20*time.Second, "How long each proof runs")
	fs.StringVar(&p.k6, "k6", "k6", "The k6 binary")
	fs.StringVar(&p.scripts, "scripts", "", "Keep the generated k6 scripts in this directory")
	fs.BoolVar(&p.dryRun, "dry-run", false, "Write the scripts and print the commands without running them")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprint(stderr, adviseUsage)
		return proveArgs{}, errUsage
	}
	if p.dryRun && p.scripts == "" {
		fmt.Fprintln(stderr, "stratus advise prove: --dry-run needs --scripts <dir> to write the scripts to")
		return proveArgs{}, errUsage
	}
	p.id = pos[0]
	return p, nil
}

// prove plans proofs of a decision's candidates, runs each k6 script in a
// load run under the plan's limits profile, and records each run as the
// option's evidence. A failed proof does not stop the others.
func prove(ctx context.Context, c ControlClient, endpoint string, p proveArgs, stdout, stderr io.Writer) error {
	body, _ := json.Marshal(map[string]any{"options": p.options, "scale": p.scale, "duration_seconds": int(p.duration.Seconds())})
	raw, err := c.AdvisePlanProofs(ctx, p.id, body)
	if err != nil {
		return err
	}
	var planned struct {
		Proofs []proofPlan `json:"proofs"`
	}
	if err := json.Unmarshal(raw, &planned); err != nil {
		return err
	}
	dir, cleanup, err := scriptDir(p.scripts)
	if err != nil {
		return err
	}
	defer cleanup()

	var failed []string
	for _, plan := range planned.Proofs {
		if !plan.Provable {
			fmt.Fprintf(stdout, "%-14s not proved: %s\n", plan.Option, plan.Reason)
			continue
		}
		path := filepath.Join(dir, "proof-"+plan.Option+".js")
		if err := os.WriteFile(path, []byte(plan.Script), 0o644); err != nil {
			return err
		}
		if p.dryRun {
			limitsPath := filepath.Join(dir, "proof-"+plan.Option+".limits.json")
			limits, _ := json.Marshal(plan.Limits)
			if err := os.WriteFile(limitsPath, limits, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%-14s stratus load run --limits %s -- %s run %s\n", plan.Option, limitsPath, p.k6, path)
			continue
		}
		line, err := proveOne(ctx, c, endpoint, p, plan, path, stderr)
		if err != nil {
			var cmdErr errCommandFailed
			if !errors.As(err, &cmdErr) {
				return fmt.Errorf("%s: %w (install k6 from https://k6.io, or pass --dry-run --scripts <dir> to run the scripts yourself)", plan.Option, err)
			}
			failed = append(failed, plan.Option)
			fmt.Fprintf(stdout, "%-14s proof failed: k6 %v; no evidence recorded\n", plan.Option, err)
			continue
		}
		fmt.Fprintln(stdout, line)
	}
	if p.dryRun {
		fmt.Fprintf(stdout, "\nAfter each run, record it: POST /_stratus/control/v1/advise/decisions/%s/evidence {option, run_id, target_rate}\n", p.id)
	} else {
		fmt.Fprintf(stdout, "\nThe ADR now carries the proofs: stratus advise adr %s\n", p.id)
	}
	if len(failed) > 0 {
		return fmt.Errorf("proofs failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func proveOne(ctx context.Context, c ControlClient, endpoint string, p proveArgs, plan proofPlan, path string, stderr io.Writer) (string, error) {
	spec, _ := json.Marshal(map[string]any{
		"name":   "advise " + p.id + " " + plan.Option,
		"limits": map[string]any{"profile": plan.Limits.Profile, "scale": plan.Limits.Scale},
	})
	// k6's own output goes to stderr, so stdout stays one line per option.
	summary, err := runWithLoad(ctx, c, endpoint, spec, []string{p.k6, "run", "--quiet", path}, stderr, stderr)
	if err != nil {
		return "", err
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(summary, &run); err != nil {
		return "", err
	}
	evidence, _ := json.Marshal(map[string]any{
		"option": plan.Option, "run_id": run.ID, "target_rate": plan.Rate, "scale": plan.Scale, "account": plan.Account,
	})
	recorded, err := c.AdviseRecordEvidence(ctx, p.id, evidence)
	if err != nil {
		return "", err
	}
	return proofLine(plan.Option, recorded)
}

// proofLine summarises the evidence recorded for option.
func proofLine(option string, decision json.RawMessage) (string, error) {
	var d struct {
		Evidence []struct {
			Option      string   `json:"option"`
			RunID       string   `json:"run_id"`
			Verdict     string   `json:"verdict"`
			TargetRate  float64  `json:"target_rate"`
			Requests    int      `json:"requests"`
			Throttled   int      `json:"throttled"`
			Errors      int      `json:"errors"`
			PerSecond   float64  `json:"per_second"`
			P99MS       float64  `json:"p99_ms"`
			ThrottledBy []string `json:"throttled_by"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(decision, &d); err != nil {
		return "", err
	}
	for _, e := range d.Evidence {
		if e.Option != option {
			continue
		}
		line := fmt.Sprintf("%-14s %-9s target %g/s, ran %.0f/s, %d requests, %d throttled, %d errors, p99 %.0f ms (%s)",
			option, e.Verdict, e.TargetRate, e.PerSecond, e.Requests, e.Throttled, e.Errors, e.P99MS, e.RunID)
		if len(e.ThrottledBy) > 0 {
			line += "; throttled by " + strings.Join(e.ThrottledBy, ", ")
		}
		if e.Verdict == "short" {
			line += "; this machine could not drive the target rate, so try --scale 0.25"
		}
		return line, nil
	}
	return option + " recorded", nil
}

// scriptDir is where proof scripts go: the directory asked for, or a
// temporary one removed afterwards.
func scriptDir(keep string) (string, func(), error) {
	if keep != "" {
		if err := os.MkdirAll(keep, 0o755); err != nil {
			return "", nil, err
		}
		return keep, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "stratus-proofs-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
