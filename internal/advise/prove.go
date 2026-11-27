package advise

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// A proof runs an option's producer path against Stratus at the stated
// peak, under the aws-default limits profile, and records what happened
// as evidence in the decision. Stratus plans the proof and reads the
// recorded run; the caller (the CLI, or an agent with a shell) runs k6,
// because a control-API server should not spawn load generators.

// Operation is one AWS API a proof script calls.
type Operation struct {
	Service string `json:"service"`
	Name    string `json:"name"`
}

// ProofTarget is what a proof aims at: its own account, the decision's
// region, and a rate and duration.
type ProofTarget struct {
	Account  string
	Region   string
	Rate     float64
	Duration time.Duration
	// Name prefixes the resources the script creates.
	Name string
}

// ProofScript is a family's proof of one option.
type ProofScript struct {
	// Operations the script calls, checked against Stratus's coverage.
	Operations []Operation
	// Script is a k6 script that reads STRATUS_ENDPOINT.
	Script string
	// Unprovable, when set, says why this option cannot be proved on
	// Stratus, e.g. a feature Stratus does not implement.
	Unprovable string
}

// Prover is implemented by a family that can prove its options with load.
type Prover interface {
	// ProofLoad is the peak rate, per second, and the region a proof of
	// these requirements aims at.
	ProofLoad(r Requirements) (peak float64, region string)
	Proof(option string, r Requirements, t ProofTarget) ProofScript
}

// Coverage says whether Stratus implements an AWS operation. The app backs
// it with internal/awsmodel, so a proof never relies on a 501.
type Coverage interface {
	Implemented(service, operation string) bool
}

// CoverageFunc adapts a function to Coverage.
type CoverageFunc func(service, operation string) bool

// Implemented calls f.
func (f CoverageFunc) Implemented(service, operation string) bool { return f(service, operation) }

// ProofPlan is how to prove one option, or why it cannot be proved.
type ProofPlan struct {
	Option          string      `json:"option"`
	Provable        bool        `json:"provable"`
	Reason          string      `json:"reason,omitempty"`
	Account         string      `json:"account"`
	Region          string      `json:"region"`
	Rate            float64     `json:"rate"`
	DurationSeconds int         `json:"duration_seconds"`
	Scale           float64     `json:"scale"`
	Operations      []Operation `json:"operations,omitempty"`
	// Limits is the limits profile to apply for the run, scaled like the
	// rate so that the ratio between load and quota holds.
	Limits ProofLimits `json:"limits"`
	Script string      `json:"script,omitempty"`
}

// ProofLimits is the limits spec a proof run applies.
type ProofLimits struct {
	Profile string  `json:"profile"`
	Scale   float64 `json:"scale"`
}

// ProofOptions narrow and size a proof plan.
type ProofOptions struct {
	// Options to prove; empty means every option that was not ruled out.
	Options []string `json:"options,omitempty"`
	// Scale multiplies the peak rate and every quota, so a laptop can
	// prove a 50,000/s design at 5,000/s; zero means 1.
	Scale float64 `json:"scale,omitempty"`
	// DurationSeconds is how long each proof runs; zero means 20.
	DurationSeconds int `json:"duration_seconds,omitempty"`
}

const (
	defaultProofSeconds = 20
	maxProofSeconds     = 600
	minProofRate        = 1
	accountDigits       = 11
	accountModulus      = 100_000_000_000 // 10^11, the digits after the leading 9
)

// PlanProofs plans a proof for each candidate option. It changes nothing.
func (d Decision) PlanProofs(f Family, coverage Coverage, opts ProofOptions) ([]ProofPlan, error) {
	if len(d.Assessments) == 0 {
		return nil, conflict("DecisionNotAssessed", "decision "+d.ID+" has no assessment of its current answers", "assess it first; proofs run the options that fit")
	}
	p, ok := f.(Prover)
	if !ok {
		return nil, conflict("ProofsUnsupported", "the "+f.ID()+" family has no load proofs", "rely on the quota checks in the assessment")
	}
	scale, seconds, err := proofSizing(opts)
	if err != nil {
		return nil, err
	}
	options, err := d.proofCandidates(opts.Options)
	if err != nil {
		return nil, err
	}
	req := d.Requirements(f)
	peak, region := p.ProofLoad(req)
	out := make([]ProofPlan, 0, len(options))
	for _, option := range options {
		target := ProofTarget{
			Account: ProofAccount(d.ID, option), Region: region,
			Rate: math.Max(minProofRate, math.Round(peak*scale)), Duration: time.Duration(seconds) * time.Second,
			Name: strings.TrimPrefix(d.ID, "adr-") + "-" + option,
		}
		script := p.Proof(option, req, target)
		plan := ProofPlan{
			Option: option, Account: target.Account, Region: region, Rate: target.Rate,
			DurationSeconds: seconds, Scale: scale, Operations: script.Operations,
			Limits: ProofLimits{Profile: "aws-default", Scale: scale},
		}
		switch missing := unsupported(script.Operations, coverage); {
		case script.Unprovable != "":
			plan.Reason = script.Unprovable
		case len(missing) > 0:
			plan.Reason = "Stratus does not implement " + strings.Join(missing, ", ") + " yet, so a proof would only measure 501s."
		default:
			plan.Provable, plan.Script = true, script.Script
		}
		out = append(out, plan)
	}
	return out, nil
}

func proofSizing(opts ProofOptions) (float64, int, error) {
	scale, seconds := opts.Scale, opts.DurationSeconds
	if scale == 0 {
		scale = 1
	}
	if seconds == 0 {
		seconds = defaultProofSeconds
	}
	if scale < 0 || scale > 1 {
		return 0, 0, invalid("InvalidProofScale", fmt.Sprintf("scale %v is outside (0, 1]", scale), "use 1 for the stated peak, or e.g. 0.1 to prove a tenth of it against quotas scaled the same way")
	}
	if seconds < 1 || seconds > maxProofSeconds {
		return 0, 0, invalid("InvalidProofDuration", fmt.Sprintf("duration %ds is outside 1 to %d seconds", seconds, maxProofSeconds), "20 to 60 seconds is usually enough to see throttling")
	}
	return scale, seconds, nil
}

func (d Decision) proofCandidates(requested []string) ([]string, error) {
	if len(requested) == 0 {
		var out []string
		for _, a := range d.Assessments {
			if a.Verdict != VerdictRuledOut {
				out = append(out, a.Option)
			}
		}
		if len(out) == 0 {
			return nil, conflict("NothingToProve", "every option of decision "+d.ID+" was ruled out", "revisit the answers marked as assumed")
		}
		return out, nil
	}
	for _, option := range requested {
		a, ok := d.assessment(option)
		if !ok {
			return nil, invalid("UnknownOption", fmt.Sprintf("%q is not an option of decision %s", option, d.ID), "prove one of: "+strings.Join(d.viable(), ", "))
		}
		if a.Verdict == VerdictRuledOut {
			return nil, conflict("OptionRuledOut", fmt.Sprintf("%s was ruled out: %s", option, firstReason(a, ReasonRuledOut)), "prove one of: "+strings.Join(d.viable(), ", "))
		}
	}
	return requested, nil
}

func unsupported(ops []Operation, coverage Coverage) []string {
	if coverage == nil {
		return nil
	}
	var out []string
	for _, op := range ops {
		if !coverage.Implemented(op.Service, op.Name) {
			out = append(out, op.Service+":"+op.Name)
		}
	}
	return out
}

// ProofAccount is the throwaway account a proof of option runs in: a
// twelve-digit ID starting with 9, derived from the decision and option,
// so proofs never share quotas or resources with each other or with the
// default account, and rerunning a proof reuses its account.
func ProofAccount(decisionID, option string) string {
	sum := sha256.Sum256([]byte(decisionID + "/" + option))
	n := binary.BigEndian.Uint64(sum[:8]) % accountModulus
	return fmt.Sprintf("9%0*d", accountDigits, n)
}

// RunResult is a finished load run as a proof reads it. The app fills it
// from internal/loadrun.
type RunResult struct {
	ID              string
	Finished        bool
	DurationSeconds float64
	Requests        int
	OK              int
	Throttled       int
	Faults          int
	Errors          int
	PerSecond       float64
	P99MS           float64
	// ThrottledBy names the limits rules that throttled requests.
	ThrottledBy []string
	// Findings are the run's findings as "severity: title".
	Findings []string
}

// RunSource reads load runs.
type RunSource interface {
	Run(id string) (RunResult, error)
}

// ProofVerdict summarises what a proof showed.
type ProofVerdict string

const (
	// ProofHeld: nearly every request succeeded at the target rate.
	ProofHeld ProofVerdict = "held"
	// ProofThrottled: quotas throttled a meaningful share of requests.
	ProofThrottled ProofVerdict = "throttled"
	// ProofFailed: requests failed for reasons other than throttling.
	ProofFailed ProofVerdict = "failed"
	// ProofShort: the load generator did not reach the target rate.
	ProofShort ProofVerdict = "short"

	// proofTolerance is the share of throttles or errors a proof that
	// held may still have, and how far short of the target rate it may
	// fall: k6 ramps up and the first and last seconds are partial.
	proofTolerance = 0.01
	proofRateFloor = 0.8
)

// Evidence is what a proof recorded for one option.
type Evidence struct {
	Option          string       `json:"option"`
	RunID           string       `json:"run_id"`
	Account         string       `json:"account,omitempty"`
	Verdict         ProofVerdict `json:"verdict"`
	TargetRate      float64      `json:"target_rate"`
	Scale           float64      `json:"scale"`
	DurationSeconds float64      `json:"duration_seconds"`
	Requests        int          `json:"requests"`
	OK              int          `json:"ok"`
	Throttled       int          `json:"throttled"`
	Errors          int          `json:"errors"`
	Faults          int          `json:"faults"`
	PerSecond       float64      `json:"per_second"`
	P99MS           float64      `json:"p99_ms"`
	ThrottledBy     []string     `json:"throttled_by,omitempty"`
	Findings        []string     `json:"findings,omitempty"`
	RecordedAt      time.Time    `json:"recorded_at"`
}

// EvidenceInput attaches a finished run to an option.
type EvidenceInput struct {
	Option     string  `json:"option"`
	RunID      string  `json:"run_id"`
	TargetRate float64 `json:"target_rate"`
	Scale      float64 `json:"scale,omitempty"`
	Account    string  `json:"account,omitempty"`
}

// NewEvidence judges a run against the rate it aimed at.
func NewEvidence(in EvidenceInput, run RunResult, now time.Time) Evidence {
	e := Evidence{
		Option: in.Option, RunID: run.ID, Account: in.Account, TargetRate: in.TargetRate, Scale: in.Scale,
		DurationSeconds: run.DurationSeconds, Requests: run.Requests, OK: run.OK, Throttled: run.Throttled,
		Errors: run.Errors, Faults: run.Faults, PerSecond: run.PerSecond, P99MS: run.P99MS,
		ThrottledBy: run.ThrottledBy, Findings: run.Findings, RecordedAt: now,
	}
	if e.Scale == 0 {
		e.Scale = 1
	}
	e.Verdict = judgeProof(e)
	return e
}

func judgeProof(e Evidence) ProofVerdict {
	if e.Requests == 0 {
		return ProofShort
	}
	n := float64(e.Requests)
	switch {
	case float64(e.Errors)/n > proofTolerance:
		return ProofFailed
	case float64(e.Throttled)/n > proofTolerance:
		return ProofThrottled
	case e.TargetRate > 0 && float64(e.OK)/e.DurationSeconds < proofRateFloor*e.TargetRate:
		return ProofShort
	}
	return ProofHeld
}

// RecordEvidence attaches a proof's evidence to an option, replacing any
// earlier proof of it. The decision must be assessed against its current
// answers (or decided), and the option must not be ruled out.
func (d *Decision) RecordEvidence(e Evidence, now time.Time) error {
	if len(d.Assessments) == 0 {
		return conflict("DecisionNotAssessed", "decision "+d.ID+" has no assessment of its current answers", "assess it, plan the proofs again and rerun them")
	}
	if _, err := d.proofCandidates([]string{e.Option}); err != nil {
		return err
	}
	kept := d.Evidence[:0:0]
	for _, old := range d.Evidence {
		if old.Option != e.Option {
			kept = append(kept, old)
		}
	}
	d.Evidence, d.UpdatedAt = append(kept, e), now
	return nil
}

// EvidenceFor returns the recorded proof of an option.
func (d Decision) EvidenceFor(option string) (Evidence, bool) {
	for _, e := range d.Evidence {
		if e.Option == option {
			return e, true
		}
	}
	return Evidence{}, false
}
