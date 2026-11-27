package advise

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/control"
)

// fakeFamily has two options: "simple" fits unless "strict" is answered
// yes, and "heavy" always fits but carries a lot of ops overhead.
type fakeFamily struct{}

func (fakeFamily) ID() string      { return "fake" }
func (fakeFamily) Title() string   { return "Fake family" }
func (fakeFamily) Summary() string { return "for tests" }
func (fakeFamily) Questions() []Question {
	return []Question{
		{ID: "strict", Prompt: "Strict?", Kind: KindBool, Default: false},
		{ID: "rate", Prompt: "Rate?", Kind: KindQuantity, Unit: "msg/s", Min: 0, Max: 1000, Default: 10.0},
		{ID: "mode", Prompt: "Mode?", Kind: KindChoice, Default: "a", Choices: []Choice{{"a", "Mode A"}, {"b", "Mode B"}}},
	}
}
func (fakeFamily) Options() []Option {
	return []Option{
		{ID: "simple", Name: "Simple", Ops: OpsProfile{Capacity: OpsScore{Score: 1, Reason: "a little"}}},
		{ID: "heavy", Name: "Heavy", Ops: OpsProfile{
			Capacity: OpsScore{Score: 3}, Patching: OpsScore{Score: 3}, Scaling: OpsScore{Score: 2},
		}},
	}
}
func (fakeFamily) Assess(r Requirements) []Assessment {
	simple := []Reason{{Kind: ReasonStrength, Text: "simple is simple"}}
	if r.Bool("strict") {
		simple = append(simple, Reason{Kind: ReasonRuledOut, Text: "simple is not strict", Citation: &Citation{QuotaID: "q1", URL: "https://docs.aws.amazon.com/x"}})
	}
	return []Assessment{Judge("simple", simple), Judge("heavy", []Reason{{Kind: ReasonCaveat, Text: "heavy is heavy"}})}
}

var t0 = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

func newFake(t *testing.T) (Decision, Family) {
	t.Helper()
	f := fakeFamily{}
	d, err := NewDecision("adr-1", "Order events", f, t0)
	if err != nil {
		t.Fatal(err)
	}
	return d, f
}

func code(err error) string {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func TestJudgeDerivesTheVerdictAndScoreFromReasons(t *testing.T) {
	cases := []struct {
		reasons []Reason
		verdict Verdict
		score   int
	}{
		{nil, VerdictFits, 100},
		{[]Reason{{Kind: ReasonStrength}}, VerdictFits, 100},
		{[]Reason{{Kind: ReasonCaveat}, {Kind: ReasonCaveat}, {Kind: ReasonStrength}}, VerdictFitsCaveats, 75},
		{[]Reason{{Kind: ReasonCaveat}, {Kind: ReasonRuledOut}}, VerdictRuledOut, 0},
		{[]Reason{{Kind: ReasonUnchecked}}, VerdictFits, 100},
		{[]Reason{{Kind: ReasonCaveat}, {Kind: ReasonStrength}, {Kind: ReasonStrength}, {Kind: ReasonStrength}, {Kind: ReasonStrength}}, VerdictFitsCaveats, 95},
	}
	for i, tc := range cases {
		a := Judge("x", tc.reasons)
		if a.Verdict != tc.verdict || a.Score != tc.score {
			t.Errorf("case %d: got %s %d, want %s %d", i, a.Verdict, a.Score, tc.verdict, tc.score)
		}
	}
}

func TestABadAnswerChangesNothing(t *testing.T) {
	d, f := newFake(t)
	err := d.Answer(f, map[string]any{"strict": true, "rate": 5000.0}, t0)
	if code(err) != "InvalidAnswer" {
		t.Fatalf("want InvalidAnswer, got %v", err)
	}
	if len(d.Answers) != 0 {
		t.Errorf("answers applied despite the error: %v", d.Answers)
	}
	if err := d.Answer(f, map[string]any{"nope": 1.0}, t0); code(err) != "UnknownQuestion" {
		t.Errorf("want UnknownQuestion, got %v", err)
	}
}

func TestAnswersAreParsedFromJSONOrCommandLineText(t *testing.T) {
	d, f := newFake(t)
	if err := d.Answer(f, map[string]any{"strict": "yes", "rate": "12.5", "mode": " B "}, t0); err != nil {
		t.Fatal(err)
	}
	if d.Answers["strict"] != true || d.Answers["rate"] != 12.5 || d.Answers["mode"] != "b" {
		t.Errorf("answers = %v", d.Answers)
	}
}

func TestChangingAnAnswerDropsTheStaleAssessment(t *testing.T) {
	d, f := newFake(t)
	if err := d.Assess(f, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Answer(f, map[string]any{"rate": 10.0}, t0); err != nil { // same as default, but now stated
		t.Fatal(err)
	}
	if d.Status != StatusDraft || d.Assessments != nil {
		t.Errorf("status %s with %d assessments, want a draft with none", d.Status, len(d.Assessments))
	}
	if err := d.Assess(f, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.Answer(f, map[string]any{"rate": 10.0}, t0); err != nil { // unchanged
		t.Fatal(err)
	}
	if d.Status != StatusAssessed {
		t.Errorf("an unchanged answer should keep the assessment, got %s", d.Status)
	}
}

func TestDecidingNeedsAnAssessmentAndAnOptionThatWasNotRuledOut(t *testing.T) {
	d, f := newFake(t)
	if err := d.Decide(f, "simple", "", t0); code(err) != "DecisionNotAssessed" {
		t.Fatalf("want DecisionNotAssessed, got %v", err)
	}
	_ = d.Answer(f, map[string]any{"strict": true}, t0)
	_ = d.Assess(f, t0)
	err := d.Decide(f, "simple", "", t0)
	if code(err) != "OptionRuledOut" || !strings.Contains(err.(*control.Error).Hint, "heavy") {
		t.Fatalf("want OptionRuledOut pointing at heavy, got %v", err)
	}
	if err := d.Decide(f, "nope", "", t0); code(err) != "UnknownOption" {
		t.Errorf("want UnknownOption, got %v", err)
	}
	if err := d.Decide(f, "heavy", "we have a platform team", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusDecided || d.Chosen != "heavy" || !d.DecidedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("decision = %+v", d)
	}
	if err := d.Answer(f, map[string]any{"strict": false}, t0); code(err) != "DecisionAlreadyDecided" {
		t.Errorf("a decided decision must keep its answers, got %v", err)
	}
}

func TestOptionsHeavierThanTheTeamCanRunGetACaveatAndRankLower(t *testing.T) {
	d, f := newFake(t)
	_ = d.Answer(f, map[string]any{OpsCapacityID: CapacityNone}, t0)
	_ = d.Assess(f, t0)
	if d.Assessments[0].Option != "simple" {
		t.Errorf("ranked first: %s", d.Assessments[0].Option)
	}
	heavy, _ := d.assessment("heavy")
	if !strings.Contains(firstReason(heavy, ReasonCaveat)+heavy.Reasons[len(heavy.Reasons)-1].Text, "Ops overhead 8/15") {
		t.Errorf("heavy reasons: %+v", heavy.Reasons)
	}
}

func TestRequirementsFillDefaultsAndRememberWhichWereAssumed(t *testing.T) {
	d, f := newFake(t)
	_ = d.Answer(f, map[string]any{"rate": 50.0}, t0)
	r := d.Requirements(f)
	if r.Number("rate") != 50 || r.Assumed("rate") {
		t.Errorf("rate = %v assumed=%v", r.Number("rate"), r.Assumed("rate"))
	}
	if r.Text("mode") != "a" || !r.Assumed("mode") || r.Text(OpsCapacityID) != CapacitySome {
		t.Errorf("defaults not filled: mode=%q ops=%q", r.Text("mode"), r.Text(OpsCapacityID))
	}
}

func TestRegistryRejectsFamiliesWithBadDefaultsOrDuplicateIDs(t *testing.T) {
	if _, err := NewRegistry(fakeFamily{}, fakeFamily{}); err == nil {
		t.Error("duplicate family accepted")
	}
	if _, err := NewRegistry(badDefault{}); err == nil {
		t.Error("family with an invalid default accepted")
	}
}

type badDefault struct{ fakeFamily }

func (badDefault) ID() string { return "bad" }
func (badDefault) Questions() []Question {
	return []Question{{ID: "q", Kind: KindChoice, Default: "z", Choices: []Choice{{"a", "A"}}}}
}

func TestTheADRShowsAssumedAnswersRankingSourcesAndTheChoice(t *testing.T) {
	d, f := newFake(t)
	_ = d.Answer(f, map[string]any{"strict": true, OpsCapacityID: CapacityPlatform}, t0)
	md := RenderADR(d, f)
	if !strings.Contains(md, "Not assessed yet") {
		t.Errorf("draft ADR:\n%s", md)
	}
	_ = d.Assess(f, t0)
	_ = d.Decide(f, "heavy", "we have a platform team.", t0)
	md = RenderADR(d, f)
	for _, want := range []string{
		"# Order events",
		"- Status: accepted",
		"| Strict? | yes | stated |",
		"| Rate? | 10 msg/s | assumed (default) |",
		"| 1 | Heavy | fits, with caveats | 85/100 | 8/15 |",
		"**Ruled out:** simple is not strict ([q1](https://docs.aws.amazon.com/x))",
		"Chosen option: **Heavy**, because we have a platform team.",
		"- Watch: heavy is heavy",
		"  - <https://docs.aws.amazon.com/x>",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("ADR lacks %q:\n%s", want, md)
		}
	}
}
