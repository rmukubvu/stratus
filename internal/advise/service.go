package advise

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/control"
)

// Service is the application service every surface (control API, CLI,
// MCP, SDKs) goes through.
type Service struct {
	repo     Repository
	families Registry
	now      func() time.Time
	newID    func() string
	mu       sync.Mutex // serialises read-modify-write of one decision
}

// NewService builds the service. now and newID may be nil.
func NewService(repo Repository, families Registry, now func() time.Time, newID func() string) *Service {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = randomID
	}
	return &Service{repo: repo, families: families, now: now, newID: newID}
}

// SetClock points the service at the emulator clock.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "adr-" + hex.EncodeToString(b)
}

// View is a decision with what a caller needs next: the questions still
// unanswered (with their defaults) and the recommended option.
type View struct {
	Decision
	FamilyTitle string     `json:"family_title"`
	Pending     []Question `json:"pending_questions"`
	Recommended string     `json:"recommended,omitempty"`
	Next        string     `json:"next"`
}

// Families lists the decision families.
func (s *Service) Families() []FamilyInfo { return s.families.Infos() }

// StartInput starts a decision.
type StartInput struct {
	Family  string         `json:"family"`
	Title   string         `json:"title,omitempty"`
	Answers map[string]any `json:"answers,omitempty"`
}

// Start creates a draft decision, optionally with first answers.
func (s *Service) Start(in StartInput) (View, error) {
	f, err := s.family(in.Family)
	if err != nil {
		return View{}, err
	}
	now := s.now().UTC()
	d, err := NewDecision(s.newID(), in.Title, f, now)
	if err != nil {
		return View{}, err
	}
	if len(in.Answers) > 0 {
		if err := d.Answer(f, in.Answers, now); err != nil {
			return View{}, err
		}
	}
	return s.save(d, f)
}

// Get returns a decision.
func (s *Service) Get(id string) (View, error) {
	d, f, err := s.load(id)
	if err != nil {
		return View{}, err
	}
	return s.view(d, f), nil
}

// List returns every decision, newest first.
func (s *Service) List() ([]View, error) {
	ds, err := s.repo.List()
	if err != nil {
		return nil, control.AsError(err)
	}
	out := make([]View, 0, len(ds))
	for _, d := range ds {
		if f, ok := s.families.Lookup(d.Family); ok {
			out = append(out, s.view(d, f))
		}
	}
	return out, nil
}

// Answer records answers to a decision's questions.
func (s *Service) Answer(id string, answers map[string]any) (View, error) {
	return s.update(id, func(d *Decision, f Family, now time.Time) error { return d.Answer(f, answers, now) })
}

// Assess judges the options against the current answers.
func (s *Service) Assess(id string) (View, error) {
	return s.update(id, func(d *Decision, f Family, now time.Time) error { return d.Assess(f, now) })
}

// DecideInput chooses an option.
type DecideInput struct {
	Option    string `json:"option"`
	Rationale string `json:"rationale,omitempty"`
}

// Decide chooses an option for an assessed decision.
func (s *Service) Decide(id string, in DecideInput) (View, error) {
	return s.update(id, func(d *Decision, f Family, now time.Time) error {
		return d.Decide(f, strings.TrimSpace(in.Option), in.Rationale, now)
	})
}

// ADR renders a decision as a Markdown architecture decision record.
func (s *Service) ADR(id string) (string, error) {
	d, f, err := s.load(id)
	if err != nil {
		return "", err
	}
	return RenderADR(d, f), nil
}

func (s *Service) update(id string, fn func(*Decision, Family, time.Time) error) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, f, err := s.load(id)
	if err != nil {
		return View{}, err
	}
	if err := fn(&d, f, s.now().UTC()); err != nil {
		return View{}, err
	}
	return s.save(d, f)
}

func (s *Service) save(d Decision, f Family) (View, error) {
	if err := s.repo.Save(d); err != nil {
		return View{}, control.AsError(err)
	}
	return s.view(d, f), nil
}

func (s *Service) load(id string) (Decision, Family, error) {
	d, ok, err := s.repo.Get(id)
	if err != nil {
		return Decision{}, nil, control.AsError(err)
	}
	if !ok {
		return Decision{}, nil, &control.Error{Kind: control.KindNotFound, Code: "DecisionNotFound", Message: "no decision " + id, Hint: "list decisions with GET /_stratus/control/v1/advise/decisions, or start one"}
	}
	f, ok := s.families.Lookup(d.Family)
	if !ok {
		return Decision{}, nil, &control.Error{Kind: control.KindConflict, Code: "UnknownDecisionFamily", Message: "decision " + id + " belongs to family " + d.Family + ", which this Stratus does not know", Hint: "use the Stratus version that created it"}
	}
	return d, f, nil
}

func (s *Service) family(id string) (Family, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	f, ok := s.families.Lookup(id)
	if !ok {
		return nil, invalid("UnknownDecisionFamily", "unknown decision family "+quoteOrEmpty(id), "valid families are: "+strings.Join(s.families.IDs(), ", "))
	}
	return f, nil
}

func quoteOrEmpty(s string) string {
	if s == "" {
		return "(none given)"
	}
	return `"` + s + `"`
}

func (s *Service) view(d Decision, f Family) View {
	v := View{Decision: d, FamilyTitle: f.Title(), Pending: []Question{}}
	for _, q := range questionsOf(f) {
		if _, ok := d.Answers[q.ID]; !ok {
			v.Pending = append(v.Pending, q)
		}
	}
	if a, ok := d.Recommended(); ok {
		v.Recommended = a.Option
	}
	switch {
	case d.Status == StatusDecided:
		v.Next = "Decided. Export the ADR (GET .../adr) and commit it with the code."
	case d.Status == StatusAssessed:
		v.Next = "Review the ranked options with the person, then decide with one that fits."
	case len(v.Pending) > 0:
		v.Next = "Ask the person the pending questions (unanswered ones use their defaults), answer, then assess."
	default:
		v.Next = "All questions answered: assess."
	}
	return v
}
