package monitoring

import (
	"fmt"
	"strings"
	"unicode"
)

// alarmRule is a parsed composite-alarm AlarmRule expression:
//
//	expr    := and ("OR" and)*
//	and     := unary ("AND" unary)*
//	unary   := "NOT" unary | primary
//	primary := "(" expr ")" | "TRUE" | "FALSE" | STATE "(" alarm ")"
//	STATE   := "ALARM" | "OK" | "INSUFFICIENT_DATA"
//
// alarm is a quoted or bare alarm name or ARN.
type alarmRule interface {
	// holds reports whether the rule is true given each child's state.
	holds(stateOf func(child string) alarmState) bool
	// visit calls fn for each child alarm the rule references.
	visit(fn func(child string))
}

type ruleConst bool

func (c ruleConst) holds(func(string) alarmState) bool { return bool(c) }
func (ruleConst) visit(func(string))                   {}

type ruleState struct {
	state alarmState
	child string
}

func (r ruleState) holds(stateOf func(string) alarmState) bool { return stateOf(r.child) == r.state }
func (r ruleState) visit(fn func(string))                      { fn(r.child) }

type ruleNot struct{ inner alarmRule }

func (r ruleNot) holds(stateOf func(string) alarmState) bool { return !r.inner.holds(stateOf) }
func (r ruleNot) visit(fn func(string))                      { r.inner.visit(fn) }

type ruleBinary struct {
	and         bool
	left, right alarmRule
}

func (r ruleBinary) holds(stateOf func(string) alarmState) bool {
	if r.and {
		return r.left.holds(stateOf) && r.right.holds(stateOf)
	}
	return r.left.holds(stateOf) || r.right.holds(stateOf)
}

func (r ruleBinary) visit(fn func(string)) { r.left.visit(fn); r.right.visit(fn) }

// ruleChildren lists the alarms rule references, in order, without repeats;
// ARNs are reduced to alarm names.
func ruleChildren(rule alarmRule) []string {
	var out []string
	seen := map[string]bool{}
	rule.visit(func(child string) {
		name := alarmNameOf(child)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	})
	return out
}

// alarmNameOf returns the alarm name of an alarm name or alarm ARN.
func alarmNameOf(ref string) string {
	if strings.HasPrefix(ref, "arn:") {
		if i := strings.Index(ref, ":alarm:"); i >= 0 {
			return ref[i+len(":alarm:"):]
		}
	}
	return ref
}

func parseAlarmRule(text string) (alarmRule, error) {
	p := &ruleParser{src: text}
	rule, err := p.expr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("unexpected %q at position %d", p.src[p.pos:], p.pos)
	}
	return rule, nil
}

type ruleParser struct {
	src string
	pos int
}

func (p *ruleParser) skipSpace() {
	for p.pos < len(p.src) && unicode.IsSpace(rune(p.src[p.pos])) {
		p.pos++
	}
}

// keyword consumes word (case-insensitively) when it stands alone next.
func (p *ruleParser) keyword(word string) bool {
	p.skipSpace()
	end := p.pos + len(word)
	if end > len(p.src) || !strings.EqualFold(p.src[p.pos:end], word) {
		return false
	}
	if end < len(p.src) && isWordByte(p.src[end]) {
		return false
	}
	p.pos = end
	return true
}

func isWordByte(b byte) bool {
	return b == '_' || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

func (p *ruleParser) expect(b byte) error {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != b {
		return fmt.Errorf("expected %q at position %d", b, p.pos)
	}
	p.pos++
	return nil
}

func (p *ruleParser) expr() (alarmRule, error) {
	left, err := p.and()
	for err == nil && p.keyword("OR") {
		var right alarmRule
		if right, err = p.and(); err == nil {
			left = ruleBinary{and: false, left: left, right: right}
		}
	}
	return left, err
}

func (p *ruleParser) and() (alarmRule, error) {
	left, err := p.unary()
	for err == nil && p.keyword("AND") {
		var right alarmRule
		if right, err = p.unary(); err == nil {
			left = ruleBinary{and: true, left: left, right: right}
		}
	}
	return left, err
}

func (p *ruleParser) unary() (alarmRule, error) {
	if p.keyword("NOT") {
		inner, err := p.unary()
		return ruleNot{inner}, err
	}
	return p.primary()
}

func (p *ruleParser) primary() (alarmRule, error) {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		inner, err := p.expr()
		if err != nil {
			return nil, err
		}
		return inner, p.expect(')')
	}
	switch {
	case p.keyword("TRUE"):
		return ruleConst(true), nil
	case p.keyword("FALSE"):
		return ruleConst(false), nil
	}
	for _, state := range []alarmState{stateInsufficientData, stateAlarm, stateOK} {
		if p.keyword(string(state)) {
			if err := p.expect('('); err != nil {
				return nil, err
			}
			child, err := p.alarmRef()
			if err != nil {
				return nil, err
			}
			return ruleState{state: state, child: child}, p.expect(')')
		}
	}
	return nil, fmt.Errorf("expected a state function, TRUE, FALSE or '(' at position %d", p.pos)
}

func (p *ruleParser) alarmRef() (string, error) {
	p.skipSpace()
	if p.pos < len(p.src) && (p.src[p.pos] == '"' || p.src[p.pos] == '\'') {
		quote := p.src[p.pos]
		end := strings.IndexByte(p.src[p.pos+1:], quote)
		if end < 0 {
			return "", fmt.Errorf("unterminated alarm name at position %d", p.pos)
		}
		name := p.src[p.pos+1 : p.pos+1+end]
		p.pos += end + 2
		return name, nil
	}
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != ')' && !unicode.IsSpace(rune(p.src[p.pos])) {
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("expected an alarm name at position %d", p.pos)
	}
	return p.src[start:p.pos], nil
}
