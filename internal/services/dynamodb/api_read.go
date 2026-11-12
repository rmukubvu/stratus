package dynamodb

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
)

// pageLimit is DynamoDB's 1 MB cap on the data one Query or Scan page reads.
const pageLimit = 1 << 20

type readInput struct {
	TableName                 string            `json:"TableName"`
	IndexName                 string            `json:"IndexName"`
	KeyConditionExpression    string            `json:"KeyConditionExpression"`
	FilterExpression          string            `json:"FilterExpression"`
	ProjectionExpression      string            `json:"ProjectionExpression"`
	ExpressionAttributeNames  map[string]string `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues map[string]any    `json:"ExpressionAttributeValues"`
	AttributesToGet           []string          `json:"AttributesToGet"`
	Limit                     *int              `json:"Limit"`
	ExclusiveStartKey         item              `json:"ExclusiveStartKey"`
	ScanIndexForward          *bool             `json:"ScanIndexForward"`
	Select                    string            `json:"Select"`
	ConsistentRead            bool              `json:"ConsistentRead"`
	ReturnConsumedCapacity    string            `json:"ReturnConsumedCapacity"`
	Segment                   *int              `json:"Segment"`
	TotalSegments             *int              `json:"TotalSegments"`
	KeyConditions             json.RawMessage   `json:"KeyConditions"`
	QueryFilter               json.RawMessage   `json:"QueryFilter"`
	ScanFilter                json.RawMessage   `json:"ScanFilter"`
}

// readPlan is a validated Query or Scan.
type readPlan struct {
	table   tableRecord
	index   *secondaryIndex
	key     keyDef // the key items are ordered and paginated by
	exprs   *compiled
	paths   []docPath
	count   bool
	limit   int
	forward bool
}

func present(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func (s *Service) plan(in readInput, query bool) (*readPlan, error) {
	if present(in.KeyConditions) || present(in.QueryFilter) || present(in.ScanFilter) {
		return nil, validation("The legacy KeyConditions, QueryFilter and ScanFilter parameters are not supported; use KeyConditionExpression and FilterExpression")
	}
	if err := validReturnConsumed(in.ReturnConsumedCapacity); err != nil {
		return nil, err
	}
	if query && in.KeyConditionExpression == "" {
		return nil, validation("Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.")
	}
	if !query && in.KeyConditionExpression != "" {
		return nil, validation("KeyConditionExpression is not supported by Scan")
	}
	// Expressions are validated before the table is looked up, as DynamoDB does.
	exprs, err := compileExpressions(expressionInputs{names: in.ExpressionAttributeNames, values: in.ExpressionAttributeValues,
		keyCond: in.KeyConditionExpression, filter: in.FilterExpression, projection: in.ProjectionExpression})
	if err != nil {
		return nil, err
	}
	t, err := s.loadTable(in.TableName)
	if err != nil {
		return nil, err
	}
	p := &readPlan{table: t, key: t.key(), exprs: exprs, forward: in.ScanIndexForward == nil || *in.ScanIndexForward}
	if in.IndexName != "" {
		ix, ok := t.index(in.IndexName)
		if !ok {
			return nil, validation("The table does not have the specified index: " + in.IndexName)
		}
		if ix.Global && in.ConsistentRead {
			return nil, validation("Consistent reads are not supported on global secondary indexes")
		}
		p.index, p.key = &ix, ix.Key
	}
	if in.Limit != nil {
		if *in.Limit < 1 {
			return nil, validation(fmt.Sprintf("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", *in.Limit))
		}
		p.limit = *in.Limit
	}
	if query {
		if err := validateKeyCondition(p.exprs.keyCond, p.key, t); err != nil {
			return nil, err
		}
		for _, path := range conditionPaths(p.exprs.filter) {
			if contains(p.key.attributes(), path[0].name) {
				return nil, validation("Filter Expression can only contain non-primary key attributes: Primary key attribute: " + path[0].name)
			}
		}
	}
	if len(in.AttributesToGet) > 0 {
		if p.paths, err = projectionOf("", in.AttributesToGet, nil); err != nil {
			return nil, err
		}
	} else {
		p.paths = p.exprs.projection
	}
	switch in.Select {
	case "", "ALL_ATTRIBUTES", "ALL_PROJECTED_ATTRIBUTES":
		if in.Select != "" && p.paths != nil {
			return nil, validation("1 validation error detected: Cannot specify the AttributesToGet when choosing to get " + in.Select)
		}
		if in.Select == "ALL_ATTRIBUTES" && p.index != nil && p.index.Global && p.index.Projection.ProjectionType != "ALL" {
			return nil, validation(fmt.Sprintf("One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported for global secondary index %s because its projection type is not ALL", p.index.Name))
		}
		if in.Select == "ALL_PROJECTED_ATTRIBUTES" && p.index == nil {
			return nil, validation("One or more parameter values were invalid: Select type ALL_PROJECTED_ATTRIBUTES is supported only for index queries or scans")
		}
	case "SPECIFIC_ATTRIBUTES":
		if p.paths == nil {
			return nil, validation("One or more parameter values were invalid: Select type SPECIFIC_ATTRIBUTES requires either ProjectionExpression or AttributesToGet")
		}
	case "COUNT":
		if p.paths != nil {
			return nil, validation("1 validation error detected: Cannot specify the AttributesToGet when choosing to get only the Count")
		}
		p.count = true
	default:
		return nil, validation("1 validation error detected: Value '" + in.Select + "' at 'select' failed to satisfy constraint: Member must satisfy enum value set: [SPECIFIC_ATTRIBUTES, COUNT, ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES]")
	}
	if in.ExclusiveStartKey != nil {
		want := append([]string{}, t.key().attributes()...)
		for _, a := range p.key.attributes() {
			if !contains(want, a) {
				want = append(want, a)
			}
		}
		for _, a := range want {
			if _, ok := in.ExclusiveStartKey[a]; !ok {
				return nil, validation("The provided starting key is invalid: The provided key element does not match the schema")
			}
		}
		if len(in.ExclusiveStartKey) != len(want) {
			return nil, validation("The provided starting key is invalid: The provided key element does not match the schema")
		}
	}
	return p, nil
}

// validateKeyCondition checks a KeyConditionExpression's shape: equality on
// the partition key, optionally AND one condition on the sort key.
func validateKeyCondition(c condition, k keyDef, t tableRecord) error {
	var parts []condition
	if and, ok := c.(andCond); ok {
		parts = []condition{and.left, and.right}
	} else {
		parts = []condition{c}
	}
	if len(parts) == 2 {
		if _, nested := parts[0].(andCond); nested {
			return validation("Conditions can be of length 1 or 2 only")
		}
		if _, nested := parts[1].(andCond); nested {
			return validation("Conditions can be of length 1 or 2 only")
		}
	}
	hashSeen := false
	for _, part := range parts {
		attr, value, op, err := keyConditionPart(part)
		if err != nil {
			return err
		}
		switch attr {
		case k.Hash:
			if op != "=" {
				return validation("Query key condition not supported")
			}
			if hashSeen {
				return validation("KeyConditionExpressions must only contain one condition per key")
			}
			hashSeen = true
		case k.Range:
			if op == "begins_with" {
				if typ, _, _ := attrType(value); typ == "N" {
					return validation("Invalid KeyConditionExpression: Incorrect operand type for operator or function; operator or function: begins_with, operand type: N")
				}
			}
		default:
			return validation("Query condition missed key schema element: " + firstMissing(k, parts))
		}
		if value != nil {
			typ, _, _ := attrType(value)
			if want := t.attributeType(attr); want != "" && typ != want {
				return validation("One or more parameter values were invalid: Condition parameter type does not match schema type")
			}
		}
	}
	if !hashSeen {
		return validation("Query condition missed key schema element: " + k.Hash)
	}
	return nil
}

func firstMissing(k keyDef, parts []condition) string {
	for _, part := range parts {
		if attr, _, _, err := keyConditionPart(part); err == nil && attr == k.Hash {
			if k.Range != "" {
				return k.Range
			}
		}
	}
	return k.Hash
}

// keyConditionPart returns the attribute one key condition constrains, the
// value it compares with and the operator.
func keyConditionPart(c condition) (string, any, string, error) {
	unsupported := validation("Query key condition not supported")
	single := func(o operand) (string, bool) {
		p, ok := o.(pathOperand)
		if !ok || len(p.path) != 1 {
			return "", false
		}
		return p.path[0].name, true
	}
	switch v := c.(type) {
	case compareCond:
		if v.op == tokNe {
			return "", nil, "", unsupported
		}
		attr, ok := single(v.left)
		lit, isLit := v.right.(literal)
		if !ok || !isLit {
			return "", nil, "", unsupported
		}
		ops := map[tokenKind]string{tokEq: "=", tokLt: "<", tokLe: "<=", tokGt: ">", tokGe: ">="}
		return attr, lit.v, ops[v.op], nil
	case betweenCond:
		attr, ok := single(v.subject)
		lo, ok1 := v.low.(literal)
		hi, ok2 := v.high.(literal)
		if !ok || !ok1 || !ok2 {
			return "", nil, "", unsupported
		}
		if c, comparable := compareValues(lo.v, hi.v); comparable && c > 0 {
			return "", nil, "", validation(fmt.Sprintf("Invalid KeyConditionExpression: The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: %s, upper bound operand: AttributeValue: %s", describeValue(lo.v), describeValue(hi.v)))
		}
		return attr, lo.v, "between", nil
	case funcCond:
		if v.name != "begins_with" || len(v.path) != 1 {
			return "", nil, "", validation("Invalid operator used in KeyConditionExpression: " + v.name)
		}
		lit, _ := v.arg.(literal)
		return v.path[0].name, lit.v, "begins_with", nil
	}
	return "", nil, "", unsupported
}

// conditionPaths lists the document paths a condition reads.
func conditionPaths(c condition) []docPath {
	var out []docPath
	addOperand := func(o operand) {
		switch v := o.(type) {
		case pathOperand:
			out = append(out, v.path)
		case sizeOperand:
			out = append(out, v.path)
		}
	}
	var walk func(condition)
	walk = func(c condition) {
		switch v := c.(type) {
		case andCond:
			walk(v.left)
			walk(v.right)
		case orCond:
			walk(v.left)
			walk(v.right)
		case notCond:
			walk(v.inner)
		case compareCond:
			addOperand(v.left)
			addOperand(v.right)
		case betweenCond:
			addOperand(v.subject)
			addOperand(v.low)
			addOperand(v.high)
		case inCond:
			addOperand(v.subject)
			for _, o := range v.list {
				addOperand(o)
			}
		case funcCond:
			out = append(out, v.path)
			if v.arg != nil {
				addOperand(v.arg)
			}
		}
	}
	if c != nil {
		walk(c)
	}
	return out
}

// pageKey is the LastEvaluatedKey for an item: the table key plus, for an
// index, the index key.
func (p *readPlan) pageKey(it item) item {
	out := keyOnly(p.table, it)
	for _, a := range p.key.attributes() {
		out[a] = it[a]
	}
	return out
}

func sameKey(a, b item) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !equalValues(v, w) {
			return false
		}
	}
	return true
}

// view is what the index holds of an item: every attribute for the table
// or an ALL index, the projected ones otherwise.
func (p *readPlan) view(it item) item {
	if p.index == nil || p.index.Projection.ProjectionType == "ALL" {
		return it
	}
	out := item{}
	for name, v := range it {
		if p.index.projects(name, p.table.key()) {
			out[name] = v
		}
	}
	return out
}

// run pages through candidates (already ordered) from ExclusiveStartKey,
// applying Limit, the 1 MB page cap, the filter and the projection.
func (p *readPlan) run(candidates []item, start item, consistent bool, capacityMode string) (map[string]any, error) {
	if start != nil {
		for i, it := range candidates {
			if sameKey(p.pageKey(it), start) {
				candidates = candidates[i+1:]
				break
			}
			if i == len(candidates)-1 {
				candidates = p.after(candidates, start)
			}
		}
	}
	items := []item{}
	scanned, bytes := 0, 0
	var last item
	for i, it := range candidates {
		if (p.limit > 0 && scanned == p.limit) || bytes >= pageLimit {
			last = p.pageKey(candidates[i-1])
			break
		}
		scanned++
		bytes += itemSize(it)
		ok, err := check(p.exprs.filter, it, "FilterExpression")
		if err != nil {
			return nil, err
		}
		if ok {
			items = append(items, project(p.view(it), p.paths))
		}
	}
	if last == nil && p.limit > 0 && scanned == p.limit && len(candidates) > scanned {
		last = p.pageKey(candidates[scanned-1])
	}
	out := map[string]any{"Count": len(items), "ScannedCount": scanned}
	if !p.count {
		out["Items"] = items
	}
	if last != nil {
		out["LastEvaluatedKey"] = last
	}
	if cc := consumed(p.table.TableName, capacityMode, readUnits(bytes, consistent), true); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// after drops the candidates up to the position start would occupy, for a
// start key whose item no longer exists.
func (p *readPlan) after(candidates []item, start item) []item {
	for i, it := range candidates {
		if p.less(start, it) {
			return candidates[i:]
		}
	}
	return nil
}

func (p *readPlan) less(a, b item) bool {
	ha, hb := canonicalKey(a[p.key.Hash]), canonicalKey(b[p.key.Hash])
	if ha != hb {
		return ha < hb
	}
	if p.key.Range != "" {
		c, _ := compareValues(a[p.key.Range], b[p.key.Range])
		if c != 0 {
			return (c < 0) == p.forward
		}
	}
	return canonicalKey(a[p.table.HashKey])+canonicalKey(a[p.table.RangeKey]) < canonicalKey(b[p.table.HashKey])+canonicalKey(b[p.table.RangeKey])
}

func (s *Service) query(r *http.Request) (any, error) {
	var in readInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	p, err := s.plan(in, true)
	if err != nil {
		return nil, err
	}
	all, err := s.tableItems(p.table)
	if err != nil {
		return nil, err
	}
	var matched []item
	for _, it := range all {
		if p.index != nil && !indexHolds(*p.index, it) {
			continue
		}
		ok, err := check(p.exprs.keyCond, it, "KeyConditionExpression")
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, it)
		}
	}
	p.order(matched)
	return p.run(matched, in.ExclusiveStartKey, in.ConsistentRead, in.ReturnConsumedCapacity)
}

// order sorts items by the read's key, honouring ScanIndexForward, with the
// table key breaking ties within an index.
func (p *readPlan) order(items []item) {
	sort.SliceStable(items, func(i, j int) bool { return p.less(items[i], items[j]) })
}

func (s *Service) scan(r *http.Request) (any, error) {
	var in readInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.ScanIndexForward != nil {
		return nil, validation("ScanIndexForward is not supported by Scan")
	}
	switch {
	case in.Segment != nil && in.TotalSegments == nil:
		return nil, validation("The TotalSegments parameter is required but was not present in the request when parameter Segment is present")
	case in.TotalSegments != nil && in.Segment == nil:
		return nil, validation("The Segment parameter is required but was not present in the request when parameter TotalSegments is present")
	case in.TotalSegments != nil && (*in.TotalSegments < 1 || *in.TotalSegments > 1000000):
		return nil, validation(fmt.Sprintf("1 validation error detected: Value '%d' at 'totalSegments' failed to satisfy constraint: Member must have value greater than or equal to 1", *in.TotalSegments))
	case in.Segment != nil && (*in.Segment < 0 || *in.Segment >= *in.TotalSegments):
		return nil, validation(fmt.Sprintf("The Segment parameter is zero-based and must be less than parameter TotalSegments: Segment: %d is not less than TotalSegments: %d", *in.Segment, *in.TotalSegments))
	}
	p, err := s.plan(in, false)
	if err != nil {
		return nil, err
	}
	all, err := s.tableItems(p.table)
	if err != nil {
		return nil, err
	}
	var candidates []item
	for _, it := range all {
		if p.index != nil && !indexHolds(*p.index, it) {
			continue
		}
		if in.TotalSegments != nil && segmentOf(it[p.table.HashKey], *in.TotalSegments) != *in.Segment {
			continue
		}
		candidates = append(candidates, it)
	}
	p.order(candidates)
	return p.run(candidates, in.ExclusiveStartKey, in.ConsistentRead, in.ReturnConsumedCapacity)
}

// segmentOf assigns a partition to one of a parallel scan's segments.
func segmentOf(hash any, total int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(canonicalKey(hash)))
	return int(h.Sum32() % uint32(total))
}
