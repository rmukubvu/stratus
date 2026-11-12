// Package tags is the resource-tag value type shared by the services: an
// ordered list of unique keys, read from AWS query-protocol forms, validated
// against AWS's limits and merged the way TagResource and UntagResource
// behave. Each service maps a *ValidationError to its own error code.
package tags

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// AWS's common limits for resource tags.
const (
	MaxPerResource = 50
	MaxKeyLength   = 128
	MaxValueLength = 256
)

// Tag is one key/value pair.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// List is a resource's tags, in the order they were first set; keys are unique.
type List []Tag

// Reason says which rule a tag list broke.
type Reason int

const (
	DuplicateKey Reason = iota + 1
	TooMany
	KeyLength
	ValueLength
	ReservedPrefix
)

// ValidationError is a tag list AWS would refuse.
type ValidationError struct {
	Reason Reason
	Key    string
	Max    int
}

func (e *ValidationError) Error() string {
	switch e.Reason {
	case DuplicateKey:
		return fmt.Sprintf("Duplicate tag keys found. Please note that Tag keys are case insensitive: %s", e.Key)
	case TooMany:
		return fmt.Sprintf("Cannot have more than %d tags on a resource", e.Max)
	case KeyLength:
		return fmt.Sprintf("Tag key must be between 1 and %d characters long", MaxKeyLength)
	case ValueLength:
		return fmt.Sprintf("Tag value must be at most %d characters long", MaxValueLength)
	case ReservedPrefix:
		return "Tag keys starting with 'aws:' are reserved for AWS use"
	}
	return "invalid tags"
}

// FromQuery reads <prefix>N.Key / <prefix>N.Value pairs (N from 1), in N
// order, as the query protocol sends a list of tags (prefix "Tags.member.").
// It reports duplicate keys rather than silently keeping one.
func FromQuery(form url.Values, prefix string) (List, error) {
	return fromQuery(form, prefix, "Key", "Value")
}

// FromQueryFields is FromQuery for services that name the fields otherwise.
func FromQueryFields(form url.Values, prefix, keyField, valueField string) (List, error) {
	return fromQuery(form, prefix, keyField, valueField)
}

func fromQuery(form url.Values, prefix, keyField, valueField string) (List, error) {
	indexes := map[int]struct{}{}
	for name := range form {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		head, _, _ := strings.Cut(rest, ".")
		if n, err := strconv.Atoi(head); err == nil && n > 0 {
			indexes[n] = struct{}{}
		}
	}
	ordered := make([]int, 0, len(indexes))
	for n := range indexes {
		ordered = append(ordered, n)
	}
	sort.Ints(ordered)

	list := make(List, 0, len(ordered))
	seen := map[string]struct{}{}
	for _, n := range ordered {
		base := prefix + strconv.Itoa(n) + "."
		key := form.Get(base + keyField)
		if _, dup := seen[key]; dup {
			return nil, &ValidationError{Reason: DuplicateKey, Key: key}
		}
		seen[key] = struct{}{}
		list = append(list, Tag{Key: key, Value: form.Get(base + valueField)})
	}
	return list, nil
}

// KeysFromQuery reads <prefix>N (N from 1), as UntagResource sends tag keys.
func KeysFromQuery(form url.Values, prefix string) []string {
	var keys []string
	for n := 1; ; n++ {
		values, ok := form[prefix+strconv.Itoa(n)]
		if !ok {
			return keys
		}
		if len(values) > 0 {
			keys = append(keys, values[0])
		}
	}
}

// Validate checks each tag's key and value, that no key uses the reserved
// "aws:" prefix, and that the list has at most max tags.
func (l List) Validate(max int) error {
	if len(l) > max {
		return &ValidationError{Reason: TooMany, Max: max}
	}
	for _, t := range l {
		if n := utf8.RuneCountInString(t.Key); n < 1 || n > MaxKeyLength {
			return &ValidationError{Reason: KeyLength, Key: t.Key}
		}
		if utf8.RuneCountInString(t.Value) > MaxValueLength {
			return &ValidationError{Reason: ValueLength, Key: t.Key}
		}
		if strings.HasPrefix(strings.ToLower(t.Key), "aws:") {
			return &ValidationError{Reason: ReservedPrefix, Key: t.Key}
		}
	}
	return nil
}

// Merge returns l with add applied: an existing key takes the new value in
// place, a new key is appended.
func (l List) Merge(add List) List {
	out := append(List(nil), l...)
	for _, t := range add {
		if i := out.index(t.Key); i >= 0 {
			out[i].Value = t.Value
			continue
		}
		out = append(out, t)
	}
	return out
}

// Without returns l minus the tags with the given keys; absent keys are ignored.
func (l List) Without(keys []string) List {
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		drop[k] = struct{}{}
	}
	out := List{}
	for _, t := range l {
		if _, ok := drop[t.Key]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// Equal reports whether l and other hold the same pairs, in any order.
func (l List) Equal(other List) bool {
	if len(l) != len(other) {
		return false
	}
	for _, t := range other {
		if i := l.index(t.Key); i < 0 || l[i].Value != t.Value {
			return false
		}
	}
	return true
}

func (l List) index(key string) int {
	for i, t := range l {
		if t.Key == key {
			return i
		}
	}
	return -1
}
