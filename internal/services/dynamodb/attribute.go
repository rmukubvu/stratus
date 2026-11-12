package dynamodb

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// item is a DynamoDB item in its wire form: attribute name to AttributeValue,
// where each AttributeValue is a one-entry object such as {"S": "x"}.
type item = map[string]any

// attrType returns an AttributeValue's type descriptor (S, N, B, BOOL,
// NULL, M, L, SS, NS, BS) and its payload.
func attrType(v any) (string, any, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", nil, false
	}
	for k, payload := range m {
		return k, payload, true
	}
	return "", nil, false
}

// standaloneMessage marks a validation message DynamoDB returns as is,
// without the context prefix callers usually add.
const standaloneMessage = "1 validation error detected: "

// valueError wraps a validateValue error with prefix, unless the message is
// one DynamoDB returns unprefixed.
func valueError(prefix string, err error) error {
	if strings.HasPrefix(err.Error(), standaloneMessage) || strings.HasPrefix(err.Error(), "Supplied AttributeValue") {
		return validation(err.Error())
	}
	return validation(prefix + err.Error())
}

// validateValue checks that v is a well-formed AttributeValue, as DynamoDB
// does before it looks at an item.
func validateValue(v any) error {
	t, payload, ok := attrType(v)
	if !ok {
		return fmt.Errorf("Supplied AttributeValue is empty, must contain exactly one of the supported datatypes")
	}
	switch t {
	case "S":
		if _, ok := payload.(string); !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
	case "N":
		s, ok := payload.(string)
		if !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
		if _, ok := parseNumber(s); !ok {
			return fmt.Errorf("The parameter cannot be converted to a numeric value: %s", s)
		}
	case "B":
		if _, ok := decodeBinary(payload); !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
	case "BOOL":
		if _, ok := payload.(bool); !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
	case "NULL":
		if b, ok := payload.(bool); !ok || !b {
			return fmt.Errorf("One or more parameter values were invalid: Null attribute value types must have the value of true")
		}
	case "M":
		m, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
		for _, child := range m {
			if err := validateValue(child); err != nil {
				return err
			}
		}
	case "L":
		l, ok := payload.([]any)
		if !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
		for _, child := range l {
			if err := validateValue(child); err != nil {
				return err
			}
		}
	case "SS", "NS", "BS":
		l, ok := payload.([]any)
		if !ok {
			return fmt.Errorf("Invalid attribute value type")
		}
		if len(l) == 0 {
			return fmt.Errorf(standaloneMessage + "One or more parameter values were invalid: An string set  may not be empty")
		}
		seen := map[string]bool{}
		for _, member := range l {
			key, err := setMemberKey(t, member)
			if err != nil {
				return err
			}
			if seen[key] {
				return fmt.Errorf("One or more parameter values were invalid: Input collection %v contains duplicates.", l)
			}
			seen[key] = true
		}
	default:
		return fmt.Errorf("Supplied AttributeValue has unknown type %s", t)
	}
	return nil
}

// setMemberKey canonicalises one member of a set, so that "1.0" and "1"
// count as the same number.
func setMemberKey(setType string, member any) (string, error) {
	switch setType {
	case "SS":
		s, ok := member.(string)
		if !ok {
			return "", fmt.Errorf("Invalid attribute value type")
		}
		return s, nil
	case "NS":
		s, _ := member.(string)
		n, ok := parseNumber(s)
		if !ok {
			return "", fmt.Errorf("The parameter cannot be converted to a numeric value: %v", member)
		}
		return n.RatString(), nil
	default:
		b, ok := decodeBinary(member)
		if !ok {
			return "", fmt.Errorf("Invalid attribute value type")
		}
		return string(b), nil
	}
}

func parseNumber(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// formatNumber renders a number the way DynamoDB returns arithmetic results:
// no exponent, no trailing zeros.
func formatNumber(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(38)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func decodeBinary(payload any) ([]byte, bool) {
	s, ok := payload.(string)
	if !ok {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	return b, err == nil
}

// compareValues orders two scalar values of the same type (S, N or B). It is
// false when they are not comparable, in which case DynamoDB's <, <=, > and
// >= are false.
func compareValues(a, b any) (int, bool) {
	ta, pa, ok1 := attrType(a)
	tb, pb, ok2 := attrType(b)
	if !ok1 || !ok2 || ta != tb {
		return 0, false
	}
	switch ta {
	case "S":
		return strings.Compare(pa.(string), pb.(string)), true
	case "N":
		x, ok1 := parseNumber(pa.(string))
		y, ok2 := parseNumber(pb.(string))
		if !ok1 || !ok2 {
			return 0, false
		}
		return x.Cmp(y), true
	case "B":
		x, ok1 := decodeBinary(pa)
		y, ok2 := decodeBinary(pb)
		if !ok1 || !ok2 {
			return 0, false
		}
		return bytes.Compare(x, y), true
	}
	return 0, false
}

// equalValues reports whether two AttributeValues are equal: numbers by
// value, sets regardless of order, documents deeply.
func equalValues(a, b any) bool {
	ta, pa, ok1 := attrType(a)
	tb, pb, ok2 := attrType(b)
	if !ok1 || !ok2 || ta != tb {
		return false
	}
	switch ta {
	case "S", "N", "B":
		c, ok := compareValues(a, b)
		return ok && c == 0
	case "BOOL", "NULL":
		return pa == pb
	case "SS", "NS", "BS":
		x, y := setKeys(ta, pa), setKeys(tb, pb)
		if len(x) != len(y) {
			return false
		}
		for k := range x {
			if !y[k] {
				return false
			}
		}
		return true
	case "L":
		x, y := pa.([]any), pb.([]any)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalValues(x[i], y[i]) {
				return false
			}
		}
		return true
	case "M":
		x, y := pa.(map[string]any), pb.(map[string]any)
		if len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !equalValues(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

func setKeys(setType string, payload any) map[string]bool {
	out := map[string]bool{}
	list, _ := payload.([]any)
	for _, m := range list {
		if k, err := setMemberKey(setType, m); err == nil {
			out[k] = true
		}
	}
	return out
}

// valueSize is DynamoDB's size of an attribute value in bytes, used for the
// 400 KB item limit and for size().
func valueSize(v any) int {
	t, payload, ok := attrType(v)
	if !ok {
		return 0
	}
	switch t {
	case "S":
		return len(payload.(string))
	case "N":
		s := strings.TrimLeft(strings.Replace(payload.(string), ".", "", 1), "-0")
		return (len(s)+1)/2 + 1
	case "B":
		b, _ := decodeBinary(payload)
		return len(b)
	case "BOOL", "NULL":
		return 1
	case "SS", "NS", "BS":
		total := 0
		for _, m := range payload.([]any) {
			total += valueSize(map[string]any{t[:1]: m})
		}
		return total
	case "L":
		total := 3
		for _, child := range payload.([]any) {
			total += valueSize(child) + 1
		}
		return total
	case "M":
		total := 3
		for k, child := range payload.(map[string]any) {
			total += len(k) + valueSize(child) + 1
		}
		return total
	}
	return 0
}

// itemSize is the size DynamoDB counts against the 400 KB item limit.
func itemSize(it item) int {
	total := 0
	for name, v := range it {
		total += len(name) + valueSize(v)
	}
	return total
}

// sizeOf is the size() function: string and binary length, element count
// of sets, lists and maps.
func sizeOf(v any) (int, bool) {
	t, payload, ok := attrType(v)
	if !ok {
		return 0, false
	}
	switch t {
	case "S":
		return len(payload.(string)), true
	case "B":
		b, _ := decodeBinary(payload)
		return len(b), true
	case "SS", "NS", "BS", "L":
		return len(payload.([]any)), true
	case "M":
		return len(payload.(map[string]any)), true
	}
	return 0, false
}

// canonicalKey encodes a key attribute so equal values encode identically
// (numbers by value) and the encoding can name a stored item.
func canonicalKey(v any) string {
	t, payload, _ := attrType(v)
	switch t {
	case "N":
		if n, ok := parseNumber(payload.(string)); ok {
			return "N:" + n.RatString()
		}
	case "S":
		raw, _ := json.Marshal(payload)
		return "S:" + string(raw)
	case "B":
		return "B:" + payload.(string)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// cloneValue deep-copies an AttributeValue or item.
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = cloneValue(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = cloneValue(child)
		}
		return out
	}
	return v
}

func cloneItem(it item) item {
	if it == nil {
		return nil
	}
	return cloneValue(it).(map[string]any)
}

// sortedNames returns an item's attribute names, sorted.
func sortedNames(it item) []string {
	names := make([]string, 0, len(it))
	for k := range it {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
