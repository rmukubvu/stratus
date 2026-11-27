package advise

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// QuestionKind says what shape of answer a question takes.
type QuestionKind string

const (
	KindChoice   QuestionKind = "choice"
	KindQuantity QuestionKind = "quantity"
	KindBool     QuestionKind = "bool"
	KindText     QuestionKind = "text"
)

// Choice is one allowed answer to a choice question.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Question asks one thing about the workload. Every question has a
// default, so a decision can be assessed with partial answers; the ADR
// says which answers were assumed.
type Question struct {
	ID     string       `json:"id"`
	Prompt string       `json:"prompt"`
	Why    string       `json:"why"`
	Kind   QuestionKind `json:"kind"`
	// Choices are the allowed values of a choice question.
	Choices []Choice `json:"choices,omitempty"`
	// Unit labels a quantity, such as "messages/s" or "KiB".
	Unit string `json:"unit,omitempty"`
	// Min and Max bound a quantity, inclusive.
	Min     float64 `json:"min,omitempty"`
	Max     float64 `json:"max,omitempty"`
	Default any     `json:"default"`
}

// Value is a validated answer: a string for choice and text questions, a
// float64 for quantities and a bool for yes/no questions.
type Value = any

// Parse validates a raw answer, as JSON decodes it or as typed on a
// command line, and returns its canonical value.
func (q Question) Parse(raw any) (Value, error) {
	switch q.Kind {
	case KindChoice:
		s, ok := asString(raw)
		if !ok {
			return nil, q.invalid(raw)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		for _, c := range q.Choices {
			if c.Value == s {
				return s, nil
			}
		}
		return nil, q.invalid(raw)
	case KindQuantity:
		f, ok := asNumber(raw)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < q.Min || (q.Max > 0 && f > q.Max) {
			return nil, q.invalid(raw)
		}
		return f, nil
	case KindBool:
		b, ok := asBool(raw)
		if !ok {
			return nil, q.invalid(raw)
		}
		return b, nil
	case KindText:
		s, ok := asString(raw)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, q.invalid(raw)
		}
		return strings.TrimSpace(s), nil
	default:
		return nil, fmt.Errorf("question %q has unknown kind %q", q.ID, q.Kind)
	}
}

// Expected describes the answers a question accepts, for error hints.
func (q Question) Expected() string {
	switch q.Kind {
	case KindChoice:
		vals := make([]string, len(q.Choices))
		for i, c := range q.Choices {
			vals[i] = c.Value
		}
		return "one of " + strings.Join(vals, ", ")
	case KindQuantity:
		if q.Max > 0 {
			return fmt.Sprintf("a number of %s from %g to %g", q.Unit, q.Min, q.Max)
		}
		return fmt.Sprintf("a number of %s, at least %g", q.Unit, q.Min)
	case KindBool:
		return "true or false"
	default:
		return "some text"
	}
}

// Label is how an answer reads in an ADR: the choice's label, the
// quantity with its unit, yes or no.
func (q Question) Label(v Value) string {
	switch val := v.(type) {
	case string:
		for _, c := range q.Choices {
			if c.Value == val {
				return c.Label
			}
		}
		return val
	case float64:
		s := strconv.FormatFloat(val, 'f', -1, 64)
		if q.Unit != "" {
			s += " " + q.Unit
		}
		return s
	case bool:
		if val {
			return "yes"
		}
		return "no"
	default:
		return fmt.Sprint(v)
	}
}

type invalidAnswer struct {
	question Question
	raw      any
}

func (e invalidAnswer) Error() string {
	return fmt.Sprintf("answer %v is not valid for %q: want %s", e.raw, e.question.ID, e.question.Expected())
}

func (q Question) invalid(raw any) error { return invalidAnswer{question: q, raw: raw} }

func asString(raw any) (string, bool) {
	s, ok := raw.(string)
	return s, ok
}

func asNumber(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asBool(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "y":
			return true, true
		case "false", "no", "n":
			return false, true
		}
	}
	return false, false
}
