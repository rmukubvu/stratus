// Package dynamodb emulates DynamoDB: tables with partition and sort keys,
// global and local secondary indexes, the expression languages (condition,
// filter, key condition, update, projection), conditional writes, Query and
// Scan with pagination, batches, transactions, TTL, tags and streams.
package dynamodb

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/dynamodbstreams"
	"github.com/stratus/internal/store"
)

const (
	tablesBucket = "dynamodb-tables"
	itemsBucket  = "dynamodb-items"
)

type Service struct {
	scope    scope.Scope
	metadata store.Store
	streams  *dynamodbstreams.Service
	now      func() time.Time
	mu       sync.Mutex

	idempotency idempotencyLedger
}

func NewService(metadata store.Store) *Service {
	return &Service{scope: scope.Default(), metadata: metadata, now: time.Now}
}

func (s *Service) SetStreams(streams *dynamodbstreams.Service) { s.streams = streams }

// SetScope sets the account and region this service instance serves.
func (s *Service) SetScope(sc scope.Scope) { s.scope = sc }

var operations = map[string]func(*Service, *http.Request) (any, error){
	"CreateTable":               (*Service).createTable,
	"DescribeTable":             (*Service).describeTable,
	"UpdateTable":               (*Service).updateTable,
	"DeleteTable":               (*Service).deleteTable,
	"ListTables":                (*Service).listTables,
	"DescribeContinuousBackups": (*Service).describeContinuousBackups,
	"UpdateContinuousBackups":   (*Service).updateContinuousBackups,
	"DescribeTimeToLive":        (*Service).describeTimeToLive,
	"UpdateTimeToLive":          (*Service).updateTimeToLive,
	"TagResource":               (*Service).tagResource,
	"UntagResource":             (*Service).untagResource,
	"ListTagsOfResource":        (*Service).listTagsOfResource,
	"DescribeEndpoints":         (*Service).describeEndpoints,
	"DescribeLimits":            (*Service).describeLimits,
	"PutItem":                   (*Service).putItem,
	"GetItem":                   (*Service).getItem,
	"UpdateItem":                (*Service).updateItem,
	"DeleteItem":                (*Service).deleteItem,
	"Query":                     (*Service).query,
	"Scan":                      (*Service).scan,
	"BatchGetItem":              (*Service).batchGetItem,
	"BatchWriteItem":            (*Service).batchWriteItem,
	"TransactWriteItems":        (*Service).transactWriteItems,
	"TransactGetItems":          (*Service).transactGetItems,
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	op, ok := operations[operation]
	if !ok {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UnknownOperationException", Message: "dynamodb operation is not implemented: " + operation}
	}
	s.mu.Lock()
	out, err := op(s, r)
	s.mu.Unlock()
	switch e := err.(type) {
	case nil:
		writeJSON(w, http.StatusOK, out)
		return nil
	case *apierror.Error:
		return &responseError{apiErr: e}
	}
	return err
}

// responseError renders an API error in DynamoDB's own shape (qualified
// __type, x-amz-crc32) while still unwrapping to the *apierror.Error the
// server records.
type responseError struct{ apiErr *apierror.Error }

func (e *responseError) Error() string { return e.apiErr.Error() }
func (e *responseError) Unwrap() error { return e.apiErr }
func (e *responseError) WriteErrorResponse(w http.ResponseWriter) {
	writeJSON(w, e.apiErr.StatusCode, map[string]any{"__type": errorTypePrefix + e.apiErr.Code, "message": e.apiErr.Message})
}

// errorTypePrefix qualifies error codes in __type, as DynamoDB does.
const errorTypePrefix = "com.amazonaws.dynamodb.v20120810#"

// decode reads the JSON request body into dst.
func decode(r *http.Request, dst any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "SerializationException", Message: err.Error()}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "SerializationException",
				Message: "Unexpected value type in payload for field " + typeErr.Field}
		}
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "SerializationException", Message: err.Error()}
	}
	return nil
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func resourceNotFound() error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Requested resource not found"}
}

func conditionFailed() error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ConditionalCheckFailedException", Message: "The conditional request failed"}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerError", Message: err.Error()}
}

// writeJSON writes a response with the x-amz-crc32 checksum DynamoDB
// clients verify.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"__type":"`+errorTypePrefix+`InternalServerError","message":"response encoding failed"}`)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.Header().Set("X-Amz-Crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Service) tableARN(name string) string { return s.scope.ARN("dynamodb", "table/"+name) }

func tableNameFromARN(arn string) string {
	const marker = ":table/"
	if idx := strings.Index(arn, marker); idx >= 0 {
		name := arn[idx+len(marker):]
		if slash := strings.Index(name, "/"); slash >= 0 {
			name = name[:slash]
		}
		return name
	}
	return arn
}

func (s *Service) emitStream(t tableRecord, oldImage, newImage item, eventName string) {
	if !t.StreamEnabled || s.streams == nil {
		return
	}
	source := newImage
	if source == nil {
		source = oldImage
	}
	_ = s.streams.AddRecord(t.TableName, keyOnly(t, source), oldImage, newImage, eventName)
}

// ---- expressions ----

// compiled holds a request's parsed expressions.
type compiled struct {
	condition  condition
	filter     condition
	keyCond    condition
	update     *updateExpr
	projection []docPath
}

type expressionInputs struct {
	names      map[string]string
	values     map[string]any
	condition  string
	filter     string
	keyCond    string
	update     string
	projection string
}

// compileExpressions parses every expression of a request and checks the
// placeholders the way DynamoDB does: each used one defined, each defined
// one used.
func compileExpressions(in expressionInputs) (*compiled, error) {
	hasExpr := in.condition != "" || in.filter != "" || in.keyCond != "" || in.update != "" || in.projection != ""
	if in.values != nil {
		if !hasExpr {
			return nil, validation("ExpressionAttributeValues can only be specified when using expressions")
		}
		if len(in.values) == 0 {
			return nil, validation("ExpressionAttributeValues must not be empty")
		}
		for k, v := range in.values {
			if err := validateValue(v); err != nil {
				if strings.HasPrefix(err.Error(), standaloneMessage) {
					return nil, validation(err.Error())
				}
				return nil, validation("ExpressionAttributeValues contains invalid value: " + err.Error() + " for key " + k)
			}
		}
	}
	if in.names != nil {
		if !hasExpr {
			return nil, validation("ExpressionAttributeNames can only be specified when using expressions")
		}
		if len(in.names) == 0 {
			return nil, validation("ExpressionAttributeNames must not be empty")
		}
	}
	ctx := newExprContext(in.names, in.values)
	out := &compiled{}
	parse := func(kind, src string, fn func(string) error) error {
		if src == "" {
			return nil
		}
		if err := fn(src); err != nil {
			var e *exprError
			if errors.As(err, &e) {
				return expressionError(kind, e)
			}
			return err
		}
		return nil
	}
	steps := []struct {
		kind, src string
		fn        func(string) error
	}{
		{"KeyConditionExpression", in.keyCond, func(src string) (err error) { out.keyCond, err = parseCondition(src, ctx); return }},
		{"UpdateExpression", in.update, func(src string) (err error) { out.update, err = parseUpdate(src, ctx); return }},
		{"ConditionExpression", in.condition, func(src string) (err error) { out.condition, err = parseCondition(src, ctx); return }},
		{"FilterExpression", in.filter, func(src string) (err error) { out.filter, err = parseCondition(src, ctx); return }},
		{"ProjectionExpression", in.projection, func(src string) (err error) { out.projection, err = parseProjection(src, ctx); return }},
	}
	for _, step := range steps {
		if err := parse(step.kind, step.src, step.fn); err != nil {
			return nil, err
		}
	}
	if err := ctx.unused(); err != nil {
		return nil, validation(err.Error())
	}
	return out, nil
}

// expressionError renders an expression error for the named expression.
func expressionError(kind string, e *exprError) error {
	msg := "Invalid " + kind + ": " + e.msg
	if e.counted && kind == "ConditionExpression" { // DynamoDB counts these only for conditions
		msg = standaloneMessage + msg
	}
	return validation(msg)
}

// check evaluates a condition, turning evaluation errors into validation
// errors.
func check(c condition, it item, kind string) (bool, error) {
	if c == nil {
		return true, nil
	}
	if it == nil {
		it = item{}
	}
	ok, err := c.eval(it)
	if err != nil {
		var e *exprError
		if errors.As(err, &e) {
			return false, expressionError(kind, e)
		}
		return false, err
	}
	return ok, nil
}

// ---- consumed capacity ----

func readUnits(size int, consistent bool) float64 {
	units := math.Ceil(float64(maxInt(size, 1)) / 4096)
	if !consistent {
		units /= 2
	}
	return units
}

func writeUnits(size int) float64 { return math.Ceil(float64(maxInt(size, 1)) / 1024) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// consumed renders ConsumedCapacity for ReturnConsumedCapacity TOTAL or
// INDEXES; it is nil for NONE.
func consumed(table string, mode string, units float64, read bool) map[string]any {
	if mode != "TOTAL" && mode != "INDEXES" {
		return nil
	}
	out := map[string]any{"TableName": table, "CapacityUnits": units}
	if read {
		out["ReadCapacityUnits"] = units
	} else {
		out["WriteCapacityUnits"] = units
	}
	if mode == "INDEXES" {
		out["Table"] = map[string]any{"CapacityUnits": units}
	}
	return out
}

func validReturnConsumed(mode string) error {
	switch mode {
	case "", "NONE", "TOTAL", "INDEXES":
		return nil
	}
	return validation("1 validation error detected: Value '" + mode + "' at 'returnConsumedCapacity' failed to satisfy constraint: Member must satisfy enum value set: [INDEXES, TOTAL, NONE]")
}
