// Package validation rejects AWS requests whose input breaks the constraints
// the AWS service model declares (required members, lengths, ranges,
// patterns and enums), with the error and message AWS's own request
// validation sends. A test that passes against Stratus with input AWS would
// refuse is a test that fails in production; this closes that gap.
package validation

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/stratus/internal/awsmodel"
)

// Violation is one broken constraint, worded as AWS words it.
type Violation struct {
	// Path is where the value sits, as AWS writes it: lower camel case
	// member names, ".member" and a 1-based index for list elements.
	Path string
	// Value is the offending value as AWS quotes it, or "null".
	Value string
	// Constraint is what the value failed, e.g. "Member must not be null".
	Constraint string
}

func (v Violation) String() string {
	value := "null"
	if v.Value != "null" {
		value = "'" + v.Value + "'"
	}
	return fmt.Sprintf("Value %s at '%s' failed to satisfy constraint: %s", value, v.Path, v.Constraint)
}

// Message is the message AWS sends for a set of violations.
func Message(violations []Violation) string {
	parts := make([]string, len(violations))
	for i, v := range violations {
		parts[i] = v.String()
	}
	noun := "errors"
	if len(violations) == 1 {
		noun = "error"
	}
	return fmt.Sprintf("%d validation %s detected: %s", len(violations), noun, strings.Join(parts, "; "))
}

// Checker walks decoded JSON input against an API's shapes. It caches the
// compiled patterns, so one Checker should serve many requests.
type Checker struct {
	patterns sync.Map // pattern string -> *regexp.Regexp, or nil when RE2 cannot compile it
}

// Check validates a decoded JSON body (from json.Decoder with UseNumber)
// against the named input shape and returns every violation, in member
// order. Members that travel outside the body are skipped: they are not in
// a JSON-protocol body.
func (c *Checker) Check(api *awsmodel.API, shapeName string, body map[string]any) []Violation {
	shape := api.Shape(shapeName)
	if shape == nil {
		return nil
	}
	var out []Violation
	c.structure(api, shape, body, "", &out)
	return out
}

func (c *Checker) structure(api *awsmodel.API, shape *awsmodel.Shape, value map[string]any, path string, out *[]Violation) {
	for _, name := range shape.Required {
		member, ok := shape.Members[name]
		if !ok || !member.Body() {
			continue
		}
		if v, present := value[wireName(name, member)]; !present || v == nil {
			*out = append(*out, Violation{Path: join(path, name), Value: "null", Constraint: "Member must not be null"})
		}
	}
	for _, name := range sortedMembers(shape) {
		member := shape.Members[name]
		if !member.Body() {
			continue
		}
		v, present := value[wireName(name, member)]
		if !present || v == nil {
			continue
		}
		c.value(api, api.Shape(member.Shape), v, join(path, name), out)
	}
}

func (c *Checker) value(api *awsmodel.API, shape *awsmodel.Shape, v any, path string, out *[]Violation) {
	if shape == nil {
		return
	}
	add := func(value, constraint string) {
		*out = append(*out, Violation{Path: path, Value: value, Constraint: constraint})
	}
	switch shape.Type {
	case "structure":
		if m, ok := v.(map[string]any); ok {
			c.structure(api, shape, m, path, out)
		}
	case "list":
		items, ok := v.([]any)
		if !ok {
			return
		}
		lengthCheck(shape, len(items), quoteList(items), add)
		for i, item := range items {
			if item != nil {
				c.value(api, api.Shape(shape.Member), item, fmt.Sprintf("%s.%d.member", path, i+1), out)
			}
		}
	case "map":
		entries, ok := v.(map[string]any)
		if !ok {
			return
		}
		lengthCheck(shape, len(entries), quoteMap(entries), add)
		for _, key := range sortedKeys(entries) {
			c.value(api, api.Shape(shape.Key), key, path+".key", out)
			if entries[key] != nil {
				c.value(api, api.Shape(shape.Value), entries[key], path+"."+key, out)
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return
		}
		lengthCheck(shape, utf8.RuneCountInString(s), s, add)
		if shape.Pattern != "" {
			if re := c.pattern(shape.Pattern); re != nil && !re.MatchString(s) {
				add(s, "Member must satisfy regular expression pattern: "+shape.Pattern)
			}
		}
		if len(shape.Enum) > 0 && !contains(shape.Enum, s) {
			add(s, "Member must satisfy enum value set: ["+strings.Join(shape.Enum, ", ")+"]")
		}
	case "blob":
		s, ok := v.(string)
		if !ok {
			return
		}
		if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
			lengthCheck(shape, len(raw), "java.nio.HeapByteBuffer", add)
		}
	case "integer", "long", "float", "double":
		n, ok := v.(json.Number)
		if !ok {
			return
		}
		f, err := strconv.ParseFloat(string(n), 64)
		if err != nil {
			return
		}
		if shape.Min != nil && f < *shape.Min {
			add(n.String(), "Member must have value greater than or equal to "+number(*shape.Min))
		}
		if shape.Max != nil && f > *shape.Max {
			add(n.String(), "Member must have value less than or equal to "+number(*shape.Max))
		}
	}
}

func lengthCheck(shape *awsmodel.Shape, n int, quoted string, add func(value, constraint string)) {
	if shape.Min != nil && float64(n) < *shape.Min {
		add(quoted, "Member must have length greater than or equal to "+number(*shape.Min))
	}
	if shape.Max != nil && float64(n) > *shape.Max {
		add(quoted, "Member must have length less than or equal to "+number(*shape.Max))
	}
}

// pattern compiles a model pattern once. Smithy patterns are ECMA 262 and
// match anywhere in the value unless anchored, which is what MatchString
// does. A pattern RE2 cannot compile (lookahead, backreferences) is skipped
// rather than enforced wrongly.
func (c *Checker) pattern(p string) *regexp.Regexp {
	if cached, ok := c.patterns.Load(p); ok {
		re, _ := cached.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(p)
	if err != nil {
		c.patterns.Store(p, (*regexp.Regexp)(nil))
		return nil
	}
	c.patterns.Store(p, re)
	return re
}

// wireName is the key a member travels under in a JSON body.
func wireName(name string, m awsmodel.Member) string {
	if m.JSONName != "" {
		return m.JSONName
	}
	if m.LocationName != "" {
		return m.LocationName
	}
	return name
}

// join appends a member to a path the way AWS's validation names it: the
// member name with its first letter lower-cased.
func join(path, member string) string {
	name := member
	if r, size := utf8.DecodeRuneInString(member); size > 0 {
		name = strings.ToLower(string(r)) + member[size:]
	}
	if path == "" {
		return name
	}
	return path + "." + name
}

func number(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func contains(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

func quoteList(items []any) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = fmt.Sprint(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func quoteMap(entries map[string]any) string {
	keys := sortedKeys(entries)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, entries[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
