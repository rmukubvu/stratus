package advise

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// provingFamily is fakeFamily with load proofs: "simple" is provable with
// one SQS call, "heavy" cannot be proved.
type provingFamily struct{ fakeFamily }

func (provingFamily) ProofLoad(r Requirements) (float64, string) {
	return r.Number("rate"), "eu-west-1"
}

func (provingFamily) Proof(option string, _ Requirements, t ProofTarget) ProofScript {
	if option == "heavy" {
		return ProofScript{Unprovable: "heavy needs a Kafka client"}
	}
	return ProofScript{
		Operations: []Operation{{Service: "sqs", Name: "SendMessage"}},
		Script:     "// " + t.Account + " " + t.Region + " " + t.Name,
	}
}

func assessed(t *testing.T, f Family, answers map[string]any) Decision {
	t.Helper()
	d, err := NewDecision("adr-p", "Proved", f, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Answer(f, answers, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Assess(f, t0); err != nil {
		t.Fatal(err)
	}
	return d
}

var everything = CoverageFunc(func(string, string) bool { return true })

func TestProofsArePlannedOnlyForAnAssessedDecisionOfAFamilyThatProves(t *testing.T) {
	f := provingFamily{}
	d, _ := NewDecision("adr-p", "", f, t0)
	if _, err := d.PlanProofs(f, everything, ProofOptions{}); code(err) != "DecisionNotAssessed" {
		t.Errorf("draft: %v", err)
	}
	plain := assessed(t, fakeFamily{}, nil)
	if _, err := plain.PlanProofs(fakeFamily{}, everything, ProofOptions{}); code(err) != "ProofsUnsupported" {
		t.Errorf("family without proofs: %v", err)
	}
}

func TestAProofPlanScalesTheRateAndQuotasAlikeInAThrowawayAccount(t *testing.T) {
	f := provingFamily{}
	d := assessed(t, f, map[string]any{"rate": 500.0})
	plans, err := d.PlanProofs(f, everything, ProofOptions{Scale: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("want a plan for both candidates, got %+v", plans)
	}
	byOption := map[string]ProofPlan{}
	for _, p := range plans {
		byOption[p.Option] = p
	}
	simple := byOption["simple"]
	if !simple.Provable || simple.Rate != 50 || simple.DurationSeconds != 20 || simple.Region != "eu-west-1" {
		t.Errorf("simple plan: %+v", simple)
	}
	if simple.Limits != (ProofLimits{Profile: "aws-default", Scale: 0.1}) {
		t.Errorf("limits %+v should scale with the rate", simple.Limits)
	}
	if !regexp.MustCompile(`^9\d{11}$`).MatchString(simple.Account) || !strings.Contains(simple.Script, simple.Account+" eu-west-1 p-simple") {
		t.Errorf("account %q script %q", simple.Account, simple.Script)
	}
	if heavy := byOption["heavy"]; heavy.Provable || heavy.Script != "" || heavy.Reason != "heavy needs a Kafka client" {
		t.Errorf("heavy plan: %+v", heavy)
	}
}

func TestEachOptionProvesInItsOwnStableAccount(t *testing.T) {
	a, b := ProofAccount("adr-1", "simple"), ProofAccount("adr-1", "heavy")
	if a == b || a != ProofAccount("adr-1", "simple") || a == ProofAccount("adr-2", "simple") {
		t.Errorf("accounts %s %s", a, b)
	}
}

func TestAProofThatWouldHitA501IsNotPlanned(t *testing.T) {
	f := provingFamily{}
	d := assessed(t, f, nil)
	none := CoverageFunc(func(string, string) bool { return false })
	plans, _ := d.PlanProofs(f, none, ProofOptions{Options: []string{"simple"}})
	if plans[0].Provable || !strings.Contains(plans[0].Reason, "sqs:SendMessage") {
		t.Errorf("plan %+v", plans[0])
	}
}

func TestProofPlansRejectRuledOutUnknownAndBadlySizedRequests(t *testing.T) {
	f := provingFamily{}
	d := assessed(t, f, map[string]any{"strict": true}) // rules simple out
	cases := map[string]ProofOptions{
		"OptionRuledOut":       {Options: []string{"simple"}},
		"UnknownOption":        {Options: []string{"nope"}},
		"InvalidProofScale":    {Scale: 2},
		"InvalidProofDuration": {DurationSeconds: 3600},
	}
	for want, opts := range cases {
		if _, err := d.PlanProofs(f, everything, opts); code(err) != want {
			t.Errorf("%+v: got %v, want %s", opts, err, want)
		}
	}
	plans, _ := d.PlanProofs(f, everything, ProofOptions{})
	if len(plans) != 1 || plans[0].Option != "heavy" {
		t.Errorf("default candidates should skip the ruled-out option: %+v", plans)
	}
}

func TestAProofIsJudgedByThrottlesErrorsAndTheRateItReached(t *testing.T) {
	base := RunResult{ID: "run-1", Finished: true, DurationSeconds: 20, Requests: 2000, OK: 2000, PerSecond: 100}
	cases := []struct {
		name string
		edit func(*RunResult)
		want ProofVerdict
	}{
		{"every request succeeded at the target", func(*RunResult) {}, ProofHeld},
		{"a few throttles at ramp-up still held", func(r *RunResult) { r.Throttled, r.OK = 10, 1990 }, ProofHeld},
		{"quotas throttled a fifth", func(r *RunResult) { r.Throttled, r.OK = 400, 1600 }, ProofThrottled},
		{"errors outrank throttles", func(r *RunResult) { r.Throttled, r.Errors, r.OK = 400, 100, 1500 }, ProofFailed},
		{"k6 could not keep up", func(r *RunResult) { r.Requests, r.OK = 1000, 1000 }, ProofShort},
		{"nothing ran", func(r *RunResult) { r.Requests, r.OK = 0, 0 }, ProofShort},
	}
	for _, tc := range cases {
		run := base
		tc.edit(&run)
		if got := NewEvidence(EvidenceInput{Option: "simple", TargetRate: 100}, run, t0).Verdict; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestRecordingAProofReplacesTheOldOneAndChangingAnAnswerDropsIt(t *testing.T) {
	f := provingFamily{}
	d := assessed(t, f, map[string]any{"rate": 100.0})
	run := RunResult{ID: "run-1", Finished: true, DurationSeconds: 20, Requests: 2000, OK: 2000}
	if err := d.RecordEvidence(NewEvidence(EvidenceInput{Option: "simple", TargetRate: 100}, run, t0), t0); err != nil {
		t.Fatal(err)
	}
	run.ID = "run-2"
	_ = d.RecordEvidence(NewEvidence(EvidenceInput{Option: "simple", TargetRate: 100}, run, t0), t0)
	if e, _ := d.EvidenceFor("simple"); len(d.Evidence) != 1 || e.RunID != "run-2" {
		t.Errorf("evidence %+v", d.Evidence)
	}
	_ = d.Answer(f, map[string]any{"rate": 200.0}, t0)
	if len(d.Evidence) != 0 {
		t.Errorf("a changed answer kept stale proofs: %+v", d.Evidence)
	}
	if err := d.RecordEvidence(NewEvidence(EvidenceInput{Option: "simple"}, run, t0), t0); code(err) != "DecisionNotAssessed" {
		t.Errorf("recorded against a draft: %v", err)
	}
}

type memRepo struct{ ds map[string]Decision }

func (m *memRepo) Save(d Decision) error { m.ds[d.ID] = d; return nil }
func (m *memRepo) Get(id string) (Decision, bool, error) {
	d, ok := m.ds[id]
	return d, ok, nil
}
func (m *memRepo) List() ([]Decision, error) { return nil, nil }

type fakeRuns map[string]RunResult

func (f fakeRuns) Run(id string) (RunResult, error) {
	r, ok := f[id]
	if !ok {
		return RunResult{}, errors.New("no run " + id)
	}
	return r, nil
}

func TestTheServiceRecordsOnlyFinishedRuns(t *testing.T) {
	reg, _ := NewRegistry(provingFamily{})
	svc := NewService(&memRepo{ds: map[string]Decision{}}, reg, func() time.Time { return t0 }, func() string { return "adr-s" })
	v, _ := svc.Start(StartInput{Family: "fake"})
	_, _ = svc.Assess(v.ID)
	in := EvidenceInput{Option: "simple", RunID: "run-1", TargetRate: 10}
	if _, err := svc.RecordEvidence(v.ID, in); code(err) != "LoadRunsUnavailable" {
		t.Errorf("no run source: %v", err)
	}
	svc.SetProofs(everything, fakeRuns{
		"run-1": {ID: "run-1", Finished: false},
		"run-2": {ID: "run-2", Finished: true, DurationSeconds: 10, Requests: 100, OK: 100},
	})
	if _, err := svc.RecordEvidence(v.ID, in); code(err) != "RunStillRecording" {
		t.Errorf("running run: %v", err)
	}
	if _, err := svc.RecordEvidence(v.ID, EvidenceInput{Option: "simple"}); code(err) != "InvalidEvidence" {
		t.Errorf("no run id: %v", err)
	}
	in.RunID = "run-2"
	got, err := svc.RecordEvidence(v.ID, in)
	if err != nil || len(got.Evidence) != 1 || got.Evidence[0].Verdict != ProofHeld {
		t.Fatalf("got %+v, %v", got.Evidence, err)
	}
	plans, err := svc.PlanProofs(v.ID, ProofOptions{})
	if err != nil || len(plans) != 2 {
		t.Errorf("plans %+v %v", plans, err)
	}
}

func TestTheADRShowsEachProofAndExplainsWhatAProofCovers(t *testing.T) {
	f := provingFamily{}
	d := assessed(t, f, map[string]any{"rate": 100.0})
	run := RunResult{ID: "run-7", Finished: true, DurationSeconds: 20, Requests: 2000, OK: 1600, Throttled: 400, PerSecond: 100, P99MS: 3,
		ThrottledBy: []string{"sns-publish-rate"}, Findings: []string{"high: Throttling with no retries"}}
	_ = d.RecordEvidence(NewEvidence(EvidenceInput{Option: "simple", TargetRate: 100, Scale: 0.5, Account: "912345678901"}, run, t0), t0)
	md := RenderADR(d, f)
	for _, want := range []string{
		"| Load proof |",
		"| throttled at 100/s |",
		"| not run |",
		"**Load proof:** throttled at 100/s for 20s (0.5 of the peak, quotas scaled alike): 2,000 requests",
		"throttled by `sns-publish-rate`",
		"(run `run-7`, account 912345678901).",
		"Finding: high: Throttling with no retries.",
		"Load proofs drove each option's producer path",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("ADR lacks %q:\n%s", want, md)
		}
	}
}
