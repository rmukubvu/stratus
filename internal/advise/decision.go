package advise

import (
	"fmt"
	"strings"
	"time"

	"github.com/stratus/internal/control"
)

// Status is where a decision is in its life.
type Status string

const (
	// StatusDraft is collecting answers; it has no current assessment.
	StatusDraft Status = "draft"
	// StatusAssessed has options judged against the current answers.
	StatusAssessed Status = "assessed"
	// StatusDecided has a chosen option; the ADR is accepted.
	StatusDecided Status = "decided"
)

// Decision is the aggregate root: one architecture decision, from the
// first answer to the chosen option. It keeps its steps in order: options
// are judged only against the current answers, changing an answer drops
// the stale assessment, and only an option that was not ruled out can be
// chosen.
type Decision struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Family      string           `json:"family"`
	Status      Status           `json:"status"`
	Answers     map[string]Value `json:"answers"`
	Assessments []Assessment     `json:"assessments,omitempty"`
	// Evidence holds load proofs of options, recorded against the
	// current answers.
	Evidence   []Evidence `json:"evidence,omitempty"`
	Chosen     string     `json:"chosen,omitempty"`
	Rationale  string     `json:"rationale,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	AssessedAt *time.Time `json:"assessed_at,omitempty"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
}

const maxTitle = 200

// NewDecision starts a draft decision.
func NewDecision(id, title string, f Family, now time.Time) (Decision, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = f.Title()
	}
	if len(title) > maxTitle {
		return Decision{}, invalid("InvalidTitle", fmt.Sprintf("title is %d characters; the most is %d", len(title), maxTitle), "use a short name such as \"Order events\"")
	}
	return Decision{
		ID: id, Title: title, Family: f.ID(), Status: StatusDraft,
		Answers: map[string]Value{}, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Answer validates and records answers. Every answer is checked before
// any is applied, so a bad one changes nothing. A changed answer drops
// the assessment and any proofs, which no longer describe the workload.
func (d *Decision) Answer(f Family, raw map[string]any, now time.Time) error {
	if d.Status == StatusDecided {
		return conflict("DecisionAlreadyDecided", "decision "+d.ID+" is decided; its answers are fixed", "start a new decision to revisit it")
	}
	questions := map[string]Question{}
	for _, q := range questionsOf(f) {
		questions[q.ID] = q
	}
	parsed := make(map[string]Value, len(raw))
	for id, v := range raw {
		q, ok := questions[id]
		if !ok {
			return invalid("UnknownQuestion", fmt.Sprintf("%q is not a question of the %s family", id, f.ID()), "valid question IDs are: "+strings.Join(questionIDs(f), ", "))
		}
		val, err := q.Parse(v)
		if err != nil {
			return invalid("InvalidAnswer", err.Error(), fmt.Sprintf("answer %s with %s", id, q.Expected()))
		}
		parsed[id] = val
	}
	changed := false
	for id, v := range parsed {
		if old, ok := d.Answers[id]; !ok || old != v {
			changed = true
		}
		d.Answers[id] = v
	}
	if changed {
		d.Status, d.Assessments, d.AssessedAt, d.Evidence = StatusDraft, nil, nil, nil
	}
	d.UpdatedAt = now
	return nil
}

// Requirements resolves the decision's answers against its family.
func (d Decision) Requirements(f Family) Requirements {
	return NewRequirements(questionsOf(f), d.Answers)
}

// Assess judges every option of the family against the current answers,
// adds the ops check and ranks the options best first.
func (d *Decision) Assess(f Family, now time.Time) error {
	if d.Status == StatusDecided {
		return conflict("DecisionAlreadyDecided", "decision "+d.ID+" is decided", "start a new decision to revisit it")
	}
	req := d.Requirements(f)
	capacity := req.Text(OpsCapacityID)
	byID := map[string]Assessment{}
	for _, a := range f.Assess(req) {
		byID[a.Option] = a
	}
	costs := map[string]Estimate{}
	if c, ok := f.(Coster); ok {
		for _, e := range c.Costs(req) {
			costs[e.Option] = e
		}
	}
	out := make([]Assessment, 0, len(f.Options()))
	for _, o := range f.Options() {
		a, ok := byID[o.ID]
		if !ok {
			a = Judge(o.ID, []Reason{{Kind: ReasonUnchecked, Text: "The family's rules did not judge this option."}})
		}
		if r := opsReason(o, capacity); r != nil {
			a = Judge(o.ID, append(a.Reasons, *r))
		}
		if e, ok := costs[o.ID]; ok {
			a.Cost = &e
		}
		out = append(out, a)
	}
	rank(out, func(id string) int {
		o, _ := optionOf(f, id)
		return o.Ops.Total()
	})
	d.Assessments, d.Status, d.UpdatedAt = out, StatusAssessed, now
	d.AssessedAt = &now
	return nil
}

// Decide chooses an option. The decision must be assessed against its
// current answers, and the option must not be ruled out.
func (d *Decision) Decide(f Family, option, rationale string, now time.Time) error {
	switch d.Status {
	case StatusDraft:
		return conflict("DecisionNotAssessed", "decision "+d.ID+" has no assessment of its current answers", "assess it first, then choose one of the options that fit")
	case StatusDecided:
		return conflict("DecisionAlreadyDecided", "decision "+d.ID+" already chose "+d.Chosen, "start a new decision to revisit it")
	}
	a, ok := d.assessment(option)
	if !ok {
		return invalid("UnknownOption", fmt.Sprintf("%q is not an option of the %s family", option, f.ID()), "choose one of: "+strings.Join(d.viable(), ", "))
	}
	if a.Verdict == VerdictRuledOut {
		return conflict("OptionRuledOut", fmt.Sprintf("%s was ruled out: %s", option, firstReason(a, ReasonRuledOut)), "choose one of: "+strings.Join(d.viable(), ", "))
	}
	d.Chosen, d.Rationale, d.Status, d.UpdatedAt = option, strings.TrimSpace(rationale), StatusDecided, now
	d.DecidedAt = &now
	return nil
}

// Recommended is the best-ranked option that was not ruled out, if any.
func (d Decision) Recommended() (Assessment, bool) {
	for _, a := range d.Assessments {
		if a.Verdict != VerdictRuledOut {
			return a, true
		}
	}
	return Assessment{}, false
}

func (d Decision) assessment(option string) (Assessment, bool) {
	for _, a := range d.Assessments {
		if a.Option == option {
			return a, true
		}
	}
	return Assessment{}, false
}

func (d Decision) viable() []string {
	var out []string
	for _, a := range d.Assessments {
		if a.Verdict != VerdictRuledOut {
			out = append(out, a.Option)
		}
	}
	if len(out) == 0 {
		return []string{"(none: every option was ruled out; revisit the answers)"}
	}
	return out
}

func firstReason(a Assessment, kind ReasonKind) string {
	for _, r := range a.Reasons {
		if r.Kind == kind {
			return r.Text
		}
	}
	return ""
}

func questionIDs(f Family) []string {
	qs := questionsOf(f)
	ids := make([]string, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
	}
	return ids
}

func invalid(code, message, hint string) *control.Error {
	return &control.Error{Kind: control.KindInvalid, Code: code, Message: message, Hint: hint}
}

func conflict(code, message, hint string) *control.Error {
	return &control.Error{Kind: control.KindConflict, Code: code, Message: message, Hint: hint}
}
