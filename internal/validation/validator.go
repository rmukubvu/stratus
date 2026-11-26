package validation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awsmodel"
)

// errorCodes is the error code each service's request validation answers
// with. Only JSON-protocol services whose framework validation AWS documents
// with the "N validation errors detected" message are listed; a service not
// listed is not validated, because a wrong error code is worse than none.
var errorCodes = map[string]string{
	"acm":             "ValidationException",
	"dynamodb":        "ValidationException",
	"dynamodbstreams": "ValidationException",
	"events":          "ValidationException",
	"kinesis":         "ValidationException",
	"kms":             "ValidationException",
	"ssm":             "ValidationException",
	"stepfunctions":   "ValidationException",
	"cognitoidp":      "InvalidParameterException",
	"logs":            "InvalidParameterException",
}

// maxBody bounds how much of a body validation reads; larger bodies are
// left to the service, which has its own size limits.
const maxBody = 16 << 20

// Validator checks AWS requests against the operation catalogue.
type Validator struct {
	checker Checker
}

// New returns a Validator.
func New() *Validator { return &Validator{} }

// Validates reports whether requests to service are validated.
func Validates(service string) bool {
	_, ok := errorCodes[service]
	return ok
}

// Validate checks a JSON-protocol request for operation of service and
// returns the AWS error for its violations, or nil. The body is restored so
// the service reads it unchanged. A request Validate cannot judge (an
// unknown operation, a body that is not a JSON object) passes through for
// the service to answer.
func (v *Validator) Validate(r *http.Request, service, operation string) error {
	code, ok := errorCodes[service]
	if !ok || r.Body == nil {
		return nil
	}
	svc, err := awsmodel.Load(service)
	if err != nil {
		return nil
	}
	api, op, ok := svc.Operation(operation)
	if !ok || op.Input == "" || api.Protocol != "json" {
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
	if err != nil || len(raw) > maxBody {
		return nil
	}
	body := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&body) != nil {
			return nil
		}
	}
	violations := v.checker.Check(api, op.Input, body)
	if len(violations) == 0 {
		return nil
	}
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: Message(violations)}
}
