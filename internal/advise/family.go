package advise

import (
	"fmt"
	"sort"
)

// Family is one kind of architecture decision, such as messaging or
// databases. It owns its questions, its candidate options and the rules
// that judge them. Adding a family means writing one of these; the
// decision core does not change.
type Family interface {
	ID() string
	Title() string
	Summary() string
	// Questions are asked in order. The core adds OpsCapacityQuestion.
	Questions() []Question
	Options() []Option
	// Assess judges every option against the requirements. It returns one
	// assessment per option; the core adds the ops check and ranks them.
	Assess(Requirements) []Assessment
}

// Option is one candidate design within a family.
type Option struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Services []string   `json:"services"`
	Summary  string     `json:"summary"`
	Ops      OpsProfile `json:"ops"`
}

// FamilyInfo describes a family for listings.
type FamilyInfo struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Summary   string     `json:"summary"`
	Options   []Option   `json:"options"`
	Questions []Question `json:"questions"`
}

// Registry holds the families Stratus can advise on.
type Registry struct {
	families map[string]Family
}

// NewRegistry builds a registry, rejecting duplicate or malformed families.
func NewRegistry(families ...Family) (Registry, error) {
	r := Registry{families: map[string]Family{}}
	for _, f := range families {
		if _, dup := r.families[f.ID()]; dup {
			return Registry{}, fmt.Errorf("advise: duplicate family %q", f.ID())
		}
		if err := checkFamily(f); err != nil {
			return Registry{}, err
		}
		r.families[f.ID()] = f
	}
	return r, nil
}

func checkFamily(f Family) error {
	seen := map[string]bool{}
	for _, q := range questionsOf(f) {
		if q.ID == "" || seen[q.ID] {
			return fmt.Errorf("advise: family %q: question ID %q is empty or repeated", f.ID(), q.ID)
		}
		seen[q.ID] = true
		if _, err := q.Parse(q.Default); err != nil {
			return fmt.Errorf("advise: family %q: default for %q: %w", f.ID(), q.ID, err)
		}
	}
	opts := map[string]bool{}
	for _, o := range f.Options() {
		if o.ID == "" || opts[o.ID] {
			return fmt.Errorf("advise: family %q: option ID %q is empty or repeated", f.ID(), o.ID)
		}
		opts[o.ID] = true
	}
	return nil
}

// Lookup returns a family by ID.
func (r Registry) Lookup(id string) (Family, bool) {
	f, ok := r.families[id]
	return f, ok
}

// IDs returns the family IDs, sorted.
func (r Registry) IDs() []string {
	ids := make([]string, 0, len(r.families))
	for id := range r.families {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Infos describes every family, sorted by ID.
func (r Registry) Infos() []FamilyInfo {
	out := make([]FamilyInfo, 0, len(r.families))
	for _, id := range r.IDs() {
		out = append(out, infoOf(r.families[id]))
	}
	return out
}

func infoOf(f Family) FamilyInfo {
	return FamilyInfo{ID: f.ID(), Title: f.Title(), Summary: f.Summary(), Options: f.Options(), Questions: questionsOf(f)}
}

// questionsOf is a family's questions followed by the core's own.
func questionsOf(f Family) []Question {
	return append(append([]Question{}, f.Questions()...), OpsCapacityQuestion())
}

func optionOf(f Family, id string) (Option, bool) {
	for _, o := range f.Options() {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}
