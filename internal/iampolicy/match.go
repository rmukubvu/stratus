package iampolicy

import "strings"

// wildcardMatch matches value against an IAM pattern where * matches any
// run of characters and ? any single character.
func wildcardMatch(pattern, value string) bool {
	p, v := 0, 0
	star, mark := -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]):
			p++
			v++
		case star >= 0:
			p = star + 1
			mark++
			v = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// matchAction compares actions case-insensitively, as IAM does.
func matchAction(pattern, action string) bool {
	return wildcardMatch(strings.ToLower(pattern), strings.ToLower(action))
}

// matchResource compares resources case-sensitively after substituting
// policy variables such as ${aws:username}.
func matchResource(pattern, resource string, ctx map[string][]string) bool {
	return wildcardMatch(substitute(pattern, ctx), resource)
}

// substitute replaces ${key} policy variables with the request's value, and
// the escapes ${*}, ${?} and ${$} with their literal character.
func substitute(pattern string, ctx map[string][]string) string {
	if !strings.Contains(pattern, "${") {
		return pattern
	}
	var b strings.Builder
	for {
		start := strings.Index(pattern, "${")
		if start < 0 {
			b.WriteString(pattern)
			return b.String()
		}
		end := strings.Index(pattern[start:], "}")
		if end < 0 {
			b.WriteString(pattern)
			return b.String()
		}
		b.WriteString(pattern[:start])
		key := pattern[start+2 : start+end]
		switch key {
		case "*", "?", "$":
			b.WriteString(key)
		default:
			// An unknown variable matches nothing, as in AWS.
			if values := lookup(ctx, key); len(values) > 0 {
				b.WriteString(values[0])
			} else {
				b.WriteString("\x00")
			}
		}
		pattern = pattern[start+end+1:]
	}
}

// lookup finds a context key case-insensitively.
func lookup(ctx map[string][]string, key string) []string {
	if v, ok := ctx[key]; ok {
		return v
	}
	for k, v := range ctx {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}
