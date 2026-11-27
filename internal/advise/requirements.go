package advise

// Requirements are a decision's answers with defaults filled in, as the
// rules read them. Families turn them into their own typed workload.
type Requirements struct {
	values  map[string]Value
	assumed map[string]bool
}

// NewRequirements resolves answers against questions: an answered
// question takes its answer, any other its default (and counts as assumed).
func NewRequirements(questions []Question, answers map[string]Value) Requirements {
	r := Requirements{values: map[string]Value{}, assumed: map[string]bool{}}
	for _, q := range questions {
		if v, ok := answers[q.ID]; ok {
			r.values[q.ID] = v
			continue
		}
		v, _ := q.Parse(q.Default) // defaults are checked by NewRegistry
		r.values[q.ID] = v
		r.assumed[q.ID] = true
	}
	return r
}

// Text returns a choice or text answer, or "" when there is none.
func (r Requirements) Text(id string) string {
	s, _ := r.values[id].(string)
	return s
}

// Number returns a quantity answer, or 0 when there is none.
func (r Requirements) Number(id string) float64 {
	f, _ := r.values[id].(float64)
	return f
}

// Bool returns a yes/no answer, or false when there is none.
func (r Requirements) Bool(id string) bool {
	b, _ := r.values[id].(bool)
	return b
}

// Assumed reports whether the question was left at its default.
func (r Requirements) Assumed(id string) bool { return r.assumed[id] }

// Value returns the raw answer.
func (r Requirements) Value(id string) Value { return r.values[id] }
