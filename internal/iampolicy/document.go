// Package iampolicy parses IAM policy documents and evaluates requests
// against them with AWS's rules: an explicit Deny wins, then any Allow,
// otherwise the request is implicitly denied.
package iampolicy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Effect is Allow or Deny.
type Effect string

const (
	Allow Effect = "Allow"
	Deny  Effect = "Deny"
)

// Document is a parsed policy document.
type Document struct {
	Version   string
	Statement []Statement
}

// Statement is one statement of a policy.
type Statement struct {
	Sid          string
	Effect       Effect
	Principal    *Principal
	NotPrincipal *Principal
	Action       []string
	NotAction    []string
	Resource     []string
	NotResource  []string
	Condition    Conditions
}

// Principal lists who a resource-based statement applies to. Wildcard is
// true for "Principal": "*".
type Principal struct {
	Wildcard  bool
	AWS       []string
	Service   []string
	Federated []string
}

// Conditions maps an operator such as StringEquals to key/values pairs.
type Conditions map[string]map[string][]string

// Parse decodes a policy document, accepting the single-value shorthand AWS
// allows for Statement, Action, Resource and condition values.
func Parse(raw string) (Document, error) {
	var wire struct {
		Version   string          `json:"Version"`
		Statement json.RawMessage `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return Document{}, fmt.Errorf("policy is not valid JSON: %w", err)
	}
	var rawStatements []json.RawMessage
	switch trimmed := strings.TrimSpace(string(wire.Statement)); {
	case trimmed == "" || trimmed == "null":
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(wire.Statement, &rawStatements); err != nil {
			return Document{}, fmt.Errorf("Statement: %w", err)
		}
	default:
		rawStatements = []json.RawMessage{wire.Statement}
	}

	doc := Document{Version: wire.Version}
	for i, rs := range rawStatements {
		st, err := parseStatement(rs)
		if err != nil {
			return Document{}, fmt.Errorf("statement %d: %w", i, err)
		}
		doc.Statement = append(doc.Statement, st)
	}
	return doc, nil
}

// MustParse parses a document known to be valid.
func MustParse(raw string) Document {
	doc, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return doc
}

func parseStatement(raw json.RawMessage) (Statement, error) {
	var wire struct {
		Sid          string                           `json:"Sid"`
		Effect       string                           `json:"Effect"`
		Principal    json.RawMessage                  `json:"Principal"`
		NotPrincipal json.RawMessage                  `json:"NotPrincipal"`
		Action       stringList                       `json:"Action"`
		NotAction    stringList                       `json:"NotAction"`
		Resource     stringList                       `json:"Resource"`
		NotResource  stringList                       `json:"NotResource"`
		Condition    map[string]map[string]stringList `json:"Condition"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Statement{}, err
	}
	st := Statement{
		Sid:         wire.Sid,
		Effect:      Effect(wire.Effect),
		Action:      wire.Action,
		NotAction:   wire.NotAction,
		Resource:    wire.Resource,
		NotResource: wire.NotResource,
	}
	if st.Effect != Allow && st.Effect != Deny {
		return Statement{}, fmt.Errorf("Effect must be Allow or Deny, got %q", wire.Effect)
	}
	if len(st.Action) == 0 && len(st.NotAction) == 0 {
		return Statement{}, fmt.Errorf("statement needs Action or NotAction")
	}
	var err error
	if st.Principal, err = parsePrincipal(wire.Principal); err != nil {
		return Statement{}, fmt.Errorf("Principal: %w", err)
	}
	if st.NotPrincipal, err = parsePrincipal(wire.NotPrincipal); err != nil {
		return Statement{}, fmt.Errorf("NotPrincipal: %w", err)
	}
	if len(wire.Condition) > 0 {
		st.Condition = Conditions{}
		for op, entries := range wire.Condition {
			st.Condition[op] = map[string][]string{}
			for key, values := range entries {
				st.Condition[op][key] = values
			}
		}
	}
	return st, nil
}

func parsePrincipal(raw json.RawMessage) (*Principal, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed == `"*"` {
		return &Principal{Wildcard: true}, nil
	}
	var wire map[string]stringList
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	p := &Principal{AWS: wire["AWS"], Service: wire["Service"], Federated: wire["Federated"]}
	for _, a := range p.AWS {
		if a == "*" {
			p.Wildcard = true
		}
	}
	return p, nil
}

// stringList decodes a JSON string or array of scalars into []string.
type stringList []string

func (l *stringList) UnmarshalJSON(raw []byte) error {
	var values []any
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
	} else {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		values = []any{v}
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case bool, float64:
			out = append(out, fmt.Sprint(t))
		case nil:
		default:
			return fmt.Errorf("unsupported value %v", v)
		}
	}
	*l = out
	return nil
}
