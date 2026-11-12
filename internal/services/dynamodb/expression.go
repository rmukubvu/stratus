package dynamodb

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---- document paths ----

type segment struct {
	name    string
	index   int
	isIndex bool
}

type docPath []segment

func (p docPath) String() string {
	var b strings.Builder
	for i, s := range p {
		switch {
		case s.isIndex:
			fmt.Fprintf(&b, "[%d]", s.index)
		case i > 0:
			b.WriteString(", " + s.name)
		default:
			b.WriteString(s.name)
		}
	}
	return "[" + b.String() + "]"
}

// get resolves the path in an item.
func (p docPath) get(it item) (any, bool) {
	if len(p) == 0 || p[0].isIndex {
		return nil, false
	}
	cur, ok := it[p[0].name]
	if !ok {
		return nil, false
	}
	for _, s := range p[1:] {
		t, payload, ok := attrType(cur)
		if !ok {
			return nil, false
		}
		switch {
		case s.isIndex && t == "L":
			list := payload.([]any)
			if s.index >= len(list) {
				return nil, false
			}
			cur = list[s.index]
		case !s.isIndex && t == "M":
			child, ok := payload.(map[string]any)[s.name]
			if !ok {
				return nil, false
			}
			cur = child
		default:
			return nil, false
		}
	}
	return cur, true
}

// errInvalidDocumentPath is DynamoDB's refusal to write through a path whose
// parent does not exist or is not a map or list.
var errInvalidDocumentPath = &exprError{msg: "The document path provided in the update expression is invalid for update"}

// set writes value at the path; a list index past the end appends.
func (p docPath) set(it item, value any) error {
	if len(p) == 1 {
		it[p[0].name] = value
		return nil
	}
	parent, ok := p[:len(p)-1].get(it)
	if !ok {
		return errInvalidDocumentPath
	}
	last := p[len(p)-1]
	t, payload, _ := attrType(parent)
	switch {
	case last.isIndex && t == "L":
		list := payload.([]any)
		if last.index >= len(list) {
			list = append(list, value)
		} else {
			list[last.index] = value
		}
		parent.(map[string]any)["L"] = list
	case !last.isIndex && t == "M":
		payload.(map[string]any)[last.name] = value
	default:
		return errInvalidDocumentPath
	}
	return nil
}

// remove deletes the path; removing a list element shifts the rest down.
func (p docPath) remove(it item) {
	if len(p) == 1 {
		delete(it, p[0].name)
		return
	}
	parent, ok := p[:len(p)-1].get(it)
	if !ok {
		return
	}
	last := p[len(p)-1]
	t, payload, _ := attrType(parent)
	switch {
	case last.isIndex && t == "L":
		list := payload.([]any)
		if last.index < len(list) {
			parent.(map[string]any)["L"] = append(list[:last.index:last.index], list[last.index+1:]...)
		}
	case !last.isIndex && t == "M":
		delete(payload.(map[string]any), last.name)
	}
}

// overlaps reports whether one path is the other or contains it.
func (p docPath) overlaps(q docPath) bool {
	n := len(p)
	if len(q) < n {
		n = len(q)
	}
	for i := 0; i < n; i++ {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

// ---- parsing context ----

// exprContext resolves #name and :value placeholders and records which were
// used, since DynamoDB refuses unused and undefined ones.
type exprContext struct {
	names      map[string]string
	values     map[string]any
	usedNames  map[string]bool
	usedValues map[string]bool
}

func newExprContext(names map[string]string, values map[string]any) *exprContext {
	return &exprContext{names: names, values: values, usedNames: map[string]bool{}, usedValues: map[string]bool{}}
}

// unused returns the error DynamoDB gives for placeholders no expression used.
func (c *exprContext) unused() error {
	var names, values []string
	for k := range c.names {
		if !c.usedNames[k] {
			names = append(names, k)
		}
	}
	for k := range c.values {
		if !c.usedValues[k] {
			values = append(values, k)
		}
	}
	sort.Strings(names)
	sort.Strings(values)
	if len(names) > 0 {
		return fmt.Errorf("Value provided in ExpressionAttributeNames unused in expressions: keys: {%s}", strings.Join(names, ", "))
	}
	if len(values) > 0 {
		return fmt.Errorf("Value provided in ExpressionAttributeValues unused in expressions: keys: {%s}", strings.Join(values, ", "))
	}
	return nil
}

type parser struct {
	src  string
	toks []token
	pos  int
	ctx  *exprContext
}

func newParser(src string, ctx *exprContext) (*parser, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	return &parser{src: src, toks: toks, ctx: ctx}, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) fail() error {
	t := p.peek()
	return syntaxError(p.src, t.text, t.pos)
}

func (p *parser) expect(kind tokenKind) (token, error) {
	if p.peek().kind != kind {
		return token{}, p.fail()
	}
	return p.next(), nil
}

func (p *parser) keyword(word string) bool {
	t := p.peek()
	if t.kind == tokIdent && strings.EqualFold(t.text, word) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) isKeyword(offset int, word string) bool {
	if p.pos+offset >= len(p.toks) {
		return false
	}
	t := p.toks[p.pos+offset]
	return t.kind == tokIdent && strings.EqualFold(t.text, word)
}

func (p *parser) done() error {
	if p.peek().kind != tokEOF {
		return p.fail()
	}
	return nil
}

func (p *parser) path() (docPath, error) {
	var out docPath
	name, err := p.pathName()
	if err != nil {
		return nil, err
	}
	out = append(out, segment{name: name})
	for {
		switch p.peek().kind {
		case tokDot:
			p.next()
			name, err := p.pathName()
			if err != nil {
				return nil, err
			}
			out = append(out, segment{name: name})
		case tokLBracket:
			p.next()
			t, err := p.expect(tokNumber)
			if err != nil {
				return nil, err
			}
			n, _ := strconv.Atoi(t.text)
			if _, err := p.expect(tokRBracket); err != nil {
				return nil, err
			}
			out = append(out, segment{index: n, isIndex: true})
		default:
			return out, nil
		}
	}
}

func (p *parser) pathName() (string, error) {
	t := p.next()
	switch t.kind {
	case tokName:
		name, ok := p.ctx.names[t.text]
		if !ok {
			return "", &exprError{msg: "An expression attribute name used in the document path is not defined; attribute name: " + t.text}
		}
		p.ctx.usedNames[t.text] = true
		return name, nil
	case tokIdent:
		if reservedWords[strings.ToUpper(t.text)] {
			return "", &exprError{msg: "Attribute name is a reserved keyword; reserved keyword: " + t.text}
		}
		return t.text, nil
	}
	return "", syntaxError(p.src, t.text, t.pos)
}

func (p *parser) valueRef() (any, error) {
	t := p.next()
	if t.kind != tokValue {
		return nil, syntaxError(p.src, t.text, t.pos)
	}
	v, ok := p.ctx.values[t.text]
	if !ok {
		return nil, &exprError{msg: "An expression attribute value used in expression is not defined; attribute value: " + t.text}
	}
	p.ctx.usedValues[t.text] = true
	return v, nil
}

// ---- conditions ----

// condition is a parsed condition, filter or key condition expression.
type condition interface {
	eval(it item) (bool, error)
}

type operand interface {
	value(it item) (any, bool)
}

type pathOperand struct{ path docPath }

func (o pathOperand) value(it item) (any, bool) { return o.path.get(it) }

type literal struct{ v any }

func (o literal) value(item) (any, bool) { return o.v, true }

type sizeOperand struct{ path docPath }

func (o sizeOperand) value(it item) (any, bool) {
	v, ok := o.path.get(it)
	if !ok {
		return nil, false
	}
	n, ok := sizeOf(v)
	if !ok {
		return nil, false
	}
	return map[string]any{"N": strconv.Itoa(n)}, true
}

type andCond struct{ left, right condition }

func (c andCond) eval(it item) (bool, error) {
	l, err := c.left.eval(it)
	if err != nil || !l {
		return false, err
	}
	return c.right.eval(it)
}

type orCond struct{ left, right condition }

func (c orCond) eval(it item) (bool, error) {
	l, err := c.left.eval(it)
	if err != nil || l {
		return l, err
	}
	return c.right.eval(it)
}

type notCond struct{ inner condition }

func (c notCond) eval(it item) (bool, error) {
	v, err := c.inner.eval(it)
	return !v, err
}

type compareCond struct {
	op          tokenKind
	left, right operand
}

func (c compareCond) eval(it item) (bool, error) {
	l, ok1 := c.left.value(it)
	r, ok2 := c.right.value(it)
	if !ok1 || !ok2 {
		return c.op == tokNe && ok1 != ok2, nil
	}
	switch c.op {
	case tokEq:
		return equalValues(l, r), nil
	case tokNe:
		return !equalValues(l, r), nil
	}
	cmp, ok := compareValues(l, r)
	if !ok {
		return false, nil
	}
	switch c.op {
	case tokLt:
		return cmp < 0, nil
	case tokLe:
		return cmp <= 0, nil
	case tokGt:
		return cmp > 0, nil
	default:
		return cmp >= 0, nil
	}
}

type betweenCond struct{ subject, low, high operand }

func (c betweenCond) eval(it item) (bool, error) {
	v, ok1 := c.subject.value(it)
	lo, ok2 := c.low.value(it)
	hi, ok3 := c.high.value(it)
	if !ok1 || !ok2 || !ok3 {
		return false, nil
	}
	if bounds, ok := compareValues(lo, hi); ok && bounds > 0 {
		return false, betweenBoundsError(lo, hi)
	}
	a, ok1 := compareValues(v, lo)
	b, ok2 := compareValues(v, hi)
	return ok1 && ok2 && a >= 0 && b <= 0, nil
}

type inCond struct {
	subject operand
	list    []operand
}

func (c inCond) eval(it item) (bool, error) {
	v, ok := c.subject.value(it)
	if !ok {
		return false, nil
	}
	for _, o := range c.list {
		if w, ok := o.value(it); ok && equalValues(v, w) {
			return true, nil
		}
	}
	return false, nil
}

type funcCond struct {
	name string
	path docPath
	arg  operand
}

func (c funcCond) eval(it item) (bool, error) {
	v, exists := c.path.get(it)
	switch c.name {
	case "attribute_exists":
		return exists, nil
	case "attribute_not_exists":
		return !exists, nil
	}
	arg, ok := c.arg.value(it)
	if !exists || !ok {
		return false, nil
	}
	switch c.name {
	case "attribute_type":
		want, _, _ := attrType(arg)
		t, _, _ := attrType(v)
		return t == want, nil
	case "begins_with":
		tv, pv, _ := attrType(v)
		ta, pa, _ := attrType(arg)
		if tv != ta {
			return false, nil
		}
		switch tv {
		case "S":
			return strings.HasPrefix(pv.(string), pa.(string)), nil
		case "B":
			x, _ := decodeBinary(pv)
			y, _ := decodeBinary(pa)
			return strings.HasPrefix(string(x), string(y)), nil
		}
		return false, nil
	case "contains":
		tv, pv, _ := attrType(v)
		ta, pa, _ := attrType(arg)
		switch tv {
		case "S":
			return ta == "S" && strings.Contains(pv.(string), pa.(string)), nil
		case "B":
			x, _ := decodeBinary(pv)
			y, _ := decodeBinary(pa)
			return ta == "B" && strings.Contains(string(x), string(y)), nil
		case "SS", "NS", "BS":
			if ta != tv[:1] {
				return false, nil
			}
			key, err := setMemberKey(tv, pa)
			return err == nil && setKeys(tv, pv)[key], nil
		case "L":
			for _, member := range pv.([]any) {
				if equalValues(member, arg) {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return false, nil
}

// parseCondition parses a condition or filter expression.
func parseCondition(src string, ctx *exprContext) (condition, error) {
	p, err := newParser(src, ctx)
	if err != nil {
		return nil, err
	}
	c, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	return c, p.done()
}

func (p *parser) orExpr() (condition, error) {
	left, err := p.andExpr()
	for err == nil && p.keyword("OR") {
		var right condition
		if right, err = p.andExpr(); err == nil {
			left = orCond{left, right}
		}
	}
	return left, err
}

func (p *parser) andExpr() (condition, error) {
	left, err := p.notExpr()
	for err == nil && p.isKeyword(0, "AND") {
		p.next()
		var right condition
		if right, err = p.notExpr(); err == nil {
			left = andCond{left, right}
		}
	}
	return left, err
}

func (p *parser) notExpr() (condition, error) {
	if p.keyword("NOT") {
		inner, err := p.notExpr()
		return notCond{inner}, err
	}
	return p.primaryCond()
}

var conditionFunctions = map[string]int{
	"attribute_exists": 1, "attribute_not_exists": 1, "attribute_type": 2, "begins_with": 2, "contains": 2,
}

func (p *parser) primaryCond() (condition, error) {
	t := p.peek()
	if t.kind == tokLParen {
		p.next()
		c, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		_, err = p.expect(tokRParen)
		return c, err
	}
	if t.kind == tokIdent && p.toks[p.pos+1].kind == tokLParen {
		name := strings.ToLower(t.text)
		if arity, ok := conditionFunctions[name]; ok {
			return p.functionCond(name, arity)
		}
		if name != "size" {
			return nil, &exprError{msg: "Invalid function name; function: " + t.text}
		}
	}
	left, err := p.operand()
	if err != nil {
		return nil, err
	}
	switch {
	case p.keyword("BETWEEN"):
		low, err := p.operand()
		if err != nil {
			return nil, err
		}
		if !p.keyword("AND") {
			return nil, p.fail()
		}
		high, err := p.operand()
		if err != nil {
			return nil, err
		}
		if l, ok := low.(literal); ok {
			if h, ok := high.(literal); ok {
				if c, ok := compareValues(l.v, h.v); ok && c > 0 {
					return nil, betweenBoundsError(l.v, h.v)
				}
			}
		}
		return betweenCond{left, low, high}, nil
	case p.keyword("IN"):
		if _, err := p.expect(tokLParen); err != nil {
			return nil, err
		}
		var list []operand
		for {
			o, err := p.operand()
			if err != nil {
				return nil, err
			}
			list = append(list, o)
			if p.peek().kind != tokComma {
				break
			}
			p.next()
		}
		if len(list) > 100 {
			return nil, &exprError{msg: "The IN operator is provided with too many operands; number of operands: " + strconv.Itoa(len(list))}
		}
		_, err := p.expect(tokRParen)
		return inCond{left, list}, err
	}
	op := p.next()
	switch op.kind {
	case tokEq, tokNe, tokLt, tokLe, tokGt, tokGe:
	default:
		return nil, syntaxError(p.src, op.text, op.pos)
	}
	right, err := p.operand()
	if err != nil {
		return nil, err
	}
	return compareCond{op.kind, left, right}, nil
}

func (p *parser) functionCond(name string, arity int) (condition, error) {
	p.next()
	p.next() // (
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	c := funcCond{name: name, path: path}
	if arity == 2 {
		if _, err := p.expect(tokComma); err != nil {
			return nil, err
		}
		if c.arg, err = p.operand(); err != nil {
			return nil, err
		}
		if name == "attribute_type" {
			if l, ok := c.arg.(literal); ok {
				t, payload, _ := attrType(l.v)
				s, _ := payload.(string)
				valid := map[string]bool{"S": true, "SS": true, "N": true, "NS": true, "B": true, "BS": true, "BOOL": true, "NULL": true, "L": true, "M": true}
				if t != "S" || !valid[s] {
					return nil, &exprError{msg: fmt.Sprintf("Invalid attribute type name found; type: %s, valid types: { B,NULL,SS,BOOL,L,BS,N,NS,S,M }", s)}
				}
				c.arg = typeNameOperand{s}
			}
		}
	}
	if _, err = p.expect(tokRParen); err != nil {
		return nil, err
	}
	if err := checkFunctionOperands(c); err != nil {
		return nil, err
	}
	return c, nil
}

// typeNameOperand carries attribute_type's type name, compared against the
// item value's type descriptor.
type typeNameOperand struct{ name string }

func (o typeNameOperand) value(item) (any, bool) { return map[string]any{o.name: nil}, true }

func (p *parser) operand() (operand, error) {
	t := p.peek()
	switch {
	case t.kind == tokValue:
		v, err := p.valueRef()
		return literal{v}, err
	case t.kind == tokIdent && strings.EqualFold(t.text, "size") && p.toks[p.pos+1].kind == tokLParen:
		p.next()
		p.next()
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		_, err = p.expect(tokRParen)
		return sizeOperand{path}, err
	case t.kind == tokIdent || t.kind == tokName:
		path, err := p.path()
		return pathOperand{path}, err
	}
	return nil, p.fail()
}

func betweenBoundsError(lo, hi any) error {
	return &exprError{counted: true, msg: fmt.Sprintf("The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: %s, upper bound operand: AttributeValue: %s", describeValue(lo), describeValue(hi))}
}

// checkFunctionOperands applies the static operand rules DynamoDB checks
// while parsing, before it looks at any item.
func checkFunctionOperands(c funcCond) error {
	if p, ok := c.arg.(pathOperand); ok && (c.name == "contains" || c.name == "begins_with") && pathsEqual(p.path, c.path) {
		return &exprError{counted: true, msg: fmt.Sprintf("The first operand must be distinct from the remaining operands for this operator or function; operator: %s, first operand: %s", c.name, c.path)}
	}
	if l, ok := c.arg.(literal); ok && c.name == "begins_with" {
		if t, _, _ := attrType(l.v); t != "S" && t != "B" {
			return &exprError{msg: "Incorrect operand type for operator or function; operator or function: begins_with, operand type: " + t}
		}
	}
	return nil
}

func pathsEqual(a, b docPath) bool { return a.String() == b.String() }

func describeValue(v any) string {
	t, payload, _ := attrType(v)
	return fmt.Sprintf("{%s:%v}", t, payload)
}

// ---- projection ----

func parseProjection(src string, ctx *exprContext) ([]docPath, error) {
	p, err := newParser(src, ctx)
	if err != nil {
		return nil, err
	}
	var out []docPath
	for {
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		for _, other := range out {
			if other.overlaps(path) {
				return nil, &exprError{msg: fmt.Sprintf("Two document paths overlap with each other; must remove or rewrite one of these paths; path one: %s, path two: %s", other, path)}
			}
		}
		out = append(out, path)
		if p.peek().kind != tokComma {
			break
		}
		p.next()
	}
	return out, p.done()
}

// project returns the parts of it the paths name, keeping their nesting.
func project(it item, paths []docPath) item {
	if paths == nil || it == nil {
		return it
	}
	out := item{}
	for _, path := range paths {
		v, ok := path.get(it)
		if !ok {
			continue
		}
		projectInto(out, path, cloneValue(v))
	}
	return out
}

func projectInto(out item, path docPath, v any) {
	var cur any = map[string]any{"M": out}
	for i, s := range path {
		last := i == len(path)-1
		t, payload, _ := attrType(cur)
		var child any
		if last {
			child = v
		} else if path[i+1].isIndex {
			child = map[string]any{"L": []any{}}
		} else {
			child = map[string]any{"M": map[string]any{}}
		}
		switch {
		case t == "M" && !s.isIndex:
			m := payload.(map[string]any)
			if existing, ok := m[s.name]; ok && !last {
				child = existing
			} else {
				m[s.name] = child
			}
		case t == "L" && s.isIndex:
			// Projected list elements are kept in order and compacted.
			list := payload.([]any)
			list = append(list, child)
			cur.(map[string]any)["L"] = list
		}
		cur = child
	}
}
