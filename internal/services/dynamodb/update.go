package dynamodb

import (
	"fmt"
	"math/big"
	"strings"
)

// updateExpr is a parsed UpdateExpression.
type updateExpr struct {
	sets    []setAction
	removes []docPath
	adds    []setOperandAction
	deletes []setOperandAction
}

type setAction struct {
	path  docPath
	value valueExpr
}

type setOperandAction struct {
	path  docPath
	value any
}

// valueExpr is the right-hand side of a SET action.
type valueExpr interface {
	resolve(it item) (any, error)
}

var (
	errMissingOperand = &exprError{msg: "The provided expression refers to an attribute that does not exist in the item"}
	errOperandType    = &exprError{msg: "An operand in the update expression has an incorrect data type"}
)

type operandValue struct{ o operand }

func (v operandValue) resolve(it item) (any, error) {
	val, ok := v.o.value(it)
	if !ok {
		return nil, errMissingOperand
	}
	return val, nil
}

type arithmetic struct {
	plus        bool
	left, right valueExpr
}

func (a arithmetic) resolve(it item) (any, error) {
	l, err := a.left.resolve(it)
	if err != nil {
		return nil, err
	}
	r, err := a.right.resolve(it)
	if err != nil {
		return nil, err
	}
	x, ok1 := numberOf(l)
	y, ok2 := numberOf(r)
	if !ok1 || !ok2 {
		return nil, errOperandType
	}
	if a.plus {
		return map[string]any{"N": formatNumber(new(big.Rat).Add(x, y))}, nil
	}
	return map[string]any{"N": formatNumber(new(big.Rat).Sub(x, y))}, nil
}

func numberOf(v any) (*big.Rat, bool) {
	t, payload, _ := attrType(v)
	if t != "N" {
		return nil, false
	}
	return parseNumber(payload.(string))
}

type ifNotExists struct {
	path     docPath
	fallback valueExpr
}

func (f ifNotExists) resolve(it item) (any, error) {
	if v, ok := f.path.get(it); ok {
		return v, nil
	}
	return f.fallback.resolve(it)
}

type listAppend struct{ first, second valueExpr }

func (f listAppend) resolve(it item) (any, error) {
	a, err := f.first.resolve(it)
	if err != nil {
		return nil, err
	}
	b, err := f.second.resolve(it)
	if err != nil {
		return nil, err
	}
	ta, pa, _ := attrType(a)
	tb, pb, _ := attrType(b)
	if ta != "L" || tb != "L" {
		return nil, errOperandType
	}
	out := append(append([]any{}, pa.([]any)...), pb.([]any)...)
	return map[string]any{"L": out}, nil
}

func parseUpdate(src string, ctx *exprContext) (*updateExpr, error) {
	p, err := newParser(src, ctx)
	if err != nil {
		return nil, err
	}
	u := &updateExpr{}
	seen := map[string]bool{}
	for p.peek().kind != tokEOF {
		t := p.next()
		clause := strings.ToUpper(t.text)
		if t.kind != tokIdent || (clause != "SET" && clause != "REMOVE" && clause != "ADD" && clause != "DELETE") {
			return nil, syntaxError(p.src, t.text, t.pos)
		}
		if seen[clause] {
			return nil, &exprError{msg: fmt.Sprintf("The %q section can only be used once in an update expression;", clause)}
		}
		seen[clause] = true
		for {
			if err := p.updateAction(clause, u); err != nil {
				return nil, err
			}
			if p.peek().kind != tokComma {
				break
			}
			p.next()
		}
	}
	if len(seen) == 0 {
		return nil, syntaxError(src, "<EOF>", len(src))
	}
	var all []docPath
	for _, s := range u.sets {
		all = append(all, s.path)
	}
	all = append(all, u.removes...)
	for _, a := range u.adds {
		all = append(all, a.path)
	}
	for _, d := range u.deletes {
		all = append(all, d.path)
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i].overlaps(all[j]) {
				return nil, &exprError{msg: fmt.Sprintf("Two document paths overlap with each other; must remove or rewrite one of these paths; path one: %s, path two: %s", all[i], all[j])}
			}
		}
	}
	return u, nil
}

func (p *parser) updateAction(clause string, u *updateExpr) error {
	path, err := p.path()
	if err != nil {
		return err
	}
	switch clause {
	case "SET":
		if _, err := p.expect(tokEq); err != nil {
			return err
		}
		v, err := p.setValue()
		if err != nil {
			return err
		}
		u.sets = append(u.sets, setAction{path, v})
	case "REMOVE":
		u.removes = append(u.removes, path)
	default:
		v, err := p.valueRef()
		if err != nil {
			return err
		}
		action := setOperandAction{path, v}
		if clause == "ADD" {
			u.adds = append(u.adds, action)
		} else {
			u.deletes = append(u.deletes, action)
		}
	}
	return nil
}

func (p *parser) setValue() (valueExpr, error) {
	left, err := p.setOperand()
	if err != nil {
		return nil, err
	}
	switch p.peek().kind {
	case tokPlus, tokMinus:
		plus := p.next().kind == tokPlus
		right, err := p.setOperand()
		if err != nil {
			return nil, err
		}
		return arithmetic{plus, left, right}, nil
	}
	return left, nil
}

func (p *parser) setOperand() (valueExpr, error) {
	t := p.peek()
	if t.kind == tokIdent && p.toks[p.pos+1].kind == tokLParen {
		name := strings.ToLower(t.text)
		p.next()
		p.next()
		switch name {
		case "if_not_exists":
			path, err := p.path()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokComma); err != nil {
				return nil, err
			}
			fallback, err := p.setValue()
			if err != nil {
				return nil, err
			}
			_, err = p.expect(tokRParen)
			return ifNotExists{path, fallback}, err
		case "list_append":
			first, err := p.setValue()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokComma); err != nil {
				return nil, err
			}
			second, err := p.setValue()
			if err != nil {
				return nil, err
			}
			_, err = p.expect(tokRParen)
			return listAppend{first, second}, err
		}
		return nil, &exprError{msg: "Invalid function name; function: " + t.text}
	}
	o, err := p.operand()
	if err != nil {
		return nil, err
	}
	if _, isSize := o.(sizeOperand); isSize {
		return nil, &exprError{msg: "The function is not allowed in an update expression; function: size"}
	}
	return operandValue{o}, nil
}

// apply returns the updated copy of it. Every right-hand side is evaluated
// against the item as it was before the update, as DynamoDB does.
func (u *updateExpr) apply(original item) (item, error) {
	out := cloneItem(original)
	resolved := make([]any, len(u.sets))
	for i, s := range u.sets {
		v, err := s.value.resolve(original)
		if err != nil {
			return nil, err
		}
		resolved[i] = cloneValue(v)
	}
	for i, s := range u.sets {
		if err := s.path.set(out, resolved[i]); err != nil {
			return nil, err
		}
	}
	for _, path := range u.removes {
		path.remove(out)
	}
	for _, a := range u.adds {
		current, exists := a.path.get(out)
		tv, _, _ := attrType(a.value)
		if tv != "N" && tv != "SS" && tv != "NS" && tv != "BS" {
			return nil, &exprError{msg: "Incorrect operand type for operator or function; operator: ADD, operand type: " + typeName(tv)}
		}
		if !exists {
			if err := a.path.set(out, cloneValue(a.value)); err != nil {
				return nil, err
			}
			continue
		}
		merged, err := addValues(current, a.value)
		if err != nil {
			return nil, err
		}
		if err := a.path.set(out, merged); err != nil {
			return nil, err
		}
	}
	for _, d := range u.deletes {
		tv, _, _ := attrType(d.value)
		if tv != "SS" && tv != "NS" && tv != "BS" {
			return nil, &exprError{msg: "Incorrect operand type for operator or function; operator: DELETE, operand type: " + typeName(tv)}
		}
		current, exists := d.path.get(out)
		if !exists {
			continue
		}
		remaining, err := deleteFromSet(current, d.value)
		if err != nil {
			return nil, err
		}
		if remaining == nil {
			d.path.remove(out)
		} else if err := d.path.set(out, remaining); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func typeName(t string) string {
	names := map[string]string{"S": "STRING", "N": "NUMBER", "B": "BINARY", "BOOL": "BOOLEAN", "NULL": "NULL", "M": "MAP", "L": "LIST",
		"SS": "STRING SET", "NS": "NUMBER SET", "BS": "BINARY SET"}
	return names[t]
}

func addValues(current, delta any) (any, error) {
	tc, pc, _ := attrType(current)
	td, pd, _ := attrType(delta)
	if tc != td {
		return nil, errOperandType
	}
	if tc == "N" {
		x, _ := parseNumber(pc.(string))
		y, _ := parseNumber(pd.(string))
		return map[string]any{"N": formatNumber(new(big.Rat).Add(x, y))}, nil
	}
	members := append([]any{}, pc.([]any)...)
	have := setKeys(tc, pc)
	for _, m := range pd.([]any) {
		key, err := setMemberKey(tc, m)
		if err != nil {
			return nil, err
		}
		if !have[key] {
			have[key] = true
			members = append(members, m)
		}
	}
	return map[string]any{tc: members}, nil
}

func deleteFromSet(current, remove any) (any, error) {
	tc, pc, _ := attrType(current)
	tr, pr, _ := attrType(remove)
	if tc != tr {
		return nil, errOperandType
	}
	drop := setKeys(tr, pr)
	var kept []any
	for _, m := range pc.([]any) {
		if key, err := setMemberKey(tc, m); err == nil && !drop[key] {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return map[string]any{tc: kept}, nil
}

// paths lists every document path an update writes, for the
// key-attribute check and UPDATED_* return values.
func (u *updateExpr) paths() []docPath {
	var out []docPath
	for _, s := range u.sets {
		out = append(out, s.path)
	}
	out = append(out, u.removes...)
	for _, a := range u.adds {
		out = append(out, a.path)
	}
	for _, d := range u.deletes {
		out = append(out, d.path)
	}
	return out
}
