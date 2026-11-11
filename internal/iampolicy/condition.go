package iampolicy

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// valueMatcher compares one request value with one policy value.
type valueMatcher func(policy, request string, ctx map[string][]string) bool

// operators maps each base condition operator to its comparison and
// whether it is a negated operator (StringNotEquals, NotIpAddress, ...).
var operators = map[string]struct {
	match   valueMatcher
	negated bool
}{
	"StringEquals":              {func(p, r string, c map[string][]string) bool { return substitute(p, c) == r }, false},
	"StringNotEquals":           {func(p, r string, c map[string][]string) bool { return substitute(p, c) == r }, true},
	"StringEqualsIgnoreCase":    {func(p, r string, c map[string][]string) bool { return strings.EqualFold(substitute(p, c), r) }, false},
	"StringNotEqualsIgnoreCase": {func(p, r string, c map[string][]string) bool { return strings.EqualFold(substitute(p, c), r) }, true},
	"StringLike":                {func(p, r string, c map[string][]string) bool { return wildcardMatch(substitute(p, c), r) }, false},
	"StringNotLike":             {func(p, r string, c map[string][]string) bool { return wildcardMatch(substitute(p, c), r) }, true},
	"ArnEquals":                 {func(p, r string, c map[string][]string) bool { return arnLike(substitute(p, c), r) }, false},
	"ArnLike":                   {func(p, r string, c map[string][]string) bool { return arnLike(substitute(p, c), r) }, false},
	"ArnNotEquals":              {func(p, r string, c map[string][]string) bool { return arnLike(substitute(p, c), r) }, true},
	"ArnNotLike":                {func(p, r string, c map[string][]string) bool { return arnLike(substitute(p, c), r) }, true},
	"NumericEquals":             {numeric(func(a, b float64) bool { return b == a }), false},
	"NumericNotEquals":          {numeric(func(a, b float64) bool { return b == a }), true},
	"NumericLessThan":           {numeric(func(a, b float64) bool { return b < a }), false},
	"NumericLessThanEquals":     {numeric(func(a, b float64) bool { return b <= a }), false},
	"NumericGreaterThan":        {numeric(func(a, b float64) bool { return b > a }), false},
	"NumericGreaterThanEquals":  {numeric(func(a, b float64) bool { return b >= a }), false},
	"DateEquals":                {date(func(a, b time.Time) bool { return b.Equal(a) }), false},
	"DateNotEquals":             {date(func(a, b time.Time) bool { return b.Equal(a) }), true},
	"DateLessThan":              {date(func(a, b time.Time) bool { return b.Before(a) }), false},
	"DateLessThanEquals":        {date(func(a, b time.Time) bool { return !b.After(a) }), false},
	"DateGreaterThan":           {date(func(a, b time.Time) bool { return b.After(a) }), false},
	"DateGreaterThanEquals":     {date(func(a, b time.Time) bool { return !b.Before(a) }), false},
	"Bool":                      {func(p, r string, _ map[string][]string) bool { return strings.EqualFold(p, r) }, false},
	"BinaryEquals":              {func(p, r string, _ map[string][]string) bool { return p == r }, false},
	"IpAddress":                 {ipMatch, false},
	"NotIpAddress":              {ipMatch, true},
}

// Satisfied reports whether every condition holds for the request context.
// Conditions are ANDed across operators and keys; the values of one key are
// ORed. Unknown operators never hold, so a statement using one is skipped
// rather than wrongly applied.
func (cs Conditions) Satisfied(ctx map[string][]string) bool {
	for op, entries := range cs {
		for key, values := range entries {
			if !evaluateCondition(op, key, values, ctx) {
				return false
			}
		}
	}
	return true
}

func evaluateCondition(op, key string, policyValues []string, ctx map[string][]string) bool {
	requestValues := lookup(ctx, key)

	if op == "Null" {
		want := len(policyValues) > 0 && strings.EqualFold(policyValues[0], "true")
		return (len(requestValues) == 0) == want
	}

	setOp := ""
	switch {
	case strings.HasPrefix(op, "ForAnyValue:"):
		setOp, op = "any", strings.TrimPrefix(op, "ForAnyValue:")
	case strings.HasPrefix(op, "ForAllValues:"):
		setOp, op = "all", strings.TrimPrefix(op, "ForAllValues:")
	}
	ifExists := strings.HasSuffix(op, "IfExists")
	op = strings.TrimSuffix(op, "IfExists")

	spec, ok := operators[op]
	if !ok {
		return false
	}
	if len(requestValues) == 0 {
		// A missing key fails a condition, except with IfExists, a negated
		// operator, or ForAllValues (vacuously true), as in AWS.
		return ifExists || setOp == "all" || (spec.negated && setOp == "")
	}

	matchesAny := func(request string) bool {
		for _, p := range policyValues {
			if spec.match(p, request, ctx) {
				return true
			}
		}
		return false
	}

	switch setOp {
	case "all":
		for _, rv := range requestValues {
			if matchesAny(rv) == spec.negated {
				return false
			}
		}
		return true
	case "any":
		for _, rv := range requestValues {
			if matchesAny(rv) != spec.negated {
				return true
			}
		}
		return false
	default:
		// A single-valued key: the first request value is the value.
		return matchesAny(requestValues[0]) != spec.negated
	}
}

// arnLike matches ARNs segment by segment, each segment a wildcard pattern.
func arnLike(pattern, arn string) bool {
	ps := strings.SplitN(pattern, ":", 6)
	as := strings.SplitN(arn, ":", 6)
	if len(ps) != 6 || len(as) != 6 {
		return wildcardMatch(pattern, arn)
	}
	for i := range ps {
		if !wildcardMatch(ps[i], as[i]) {
			return false
		}
	}
	return true
}

func numeric(cmp func(policy, request float64) bool) valueMatcher {
	return func(p, r string, _ map[string][]string) bool {
		pv, err1 := strconv.ParseFloat(p, 64)
		rv, err2 := strconv.ParseFloat(r, 64)
		return err1 == nil && err2 == nil && cmp(pv, rv)
	}
}

func date(cmp func(policy, request time.Time) bool) valueMatcher {
	return func(p, r string, _ map[string][]string) bool {
		pv, ok1 := parseDate(p)
		rv, ok2 := parseDate(r)
		return ok1 && ok2 && cmp(pv, rv)
	}
}

func parseDate(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), true
	}
	return time.Time{}, false
}

func ipMatch(p, r string, _ map[string][]string) bool {
	ip := net.ParseIP(r)
	if ip == nil {
		return false
	}
	if !strings.Contains(p, "/") {
		return ip.Equal(net.ParseIP(p))
	}
	_, network, err := net.ParseCIDR(p)
	return err == nil && network.Contains(ip)
}
