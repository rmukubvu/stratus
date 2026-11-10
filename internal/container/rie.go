package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

// rieInvokePath is the Runtime Interface Emulator's invoke endpoint.
const rieInvokePath = "/2015-03-31/functions/function/invocations"

// functionErrorUnhandled is what Lambda reports in X-Amz-Function-Error for
// every error raised by current runtimes, crashes and timeouts included.
const functionErrorUnhandled = "Unhandled"

// rieOutcome is one emulator response, before it is mapped onto Lambda's
// Invoke result.
type rieOutcome struct {
	Status int
	Body   []byte
}

// errRuntimeTimedOut marks an invocation the emulator stopped at the
// function's timeout. The container is replaced afterwards, as Lambda does.
type errRuntimeTimedOut struct{ payload []byte }

func (errRuntimeTimedOut) Error() string { return "lambda invocation timed out" }

// toInvokeResult maps the emulator's response onto what Lambda's Invoke
// returns. The emulator does not send X-Amz-Function-Error, so errors are
// recognised from the status and body:
//
//   - 200 with the body "Task timed out after N.00 seconds": timeout.
//   - 502 with an error document: the handler raised, or the runtime exited.
//   - 200 with an error document: older emulators report handler errors so.
//   - anything else from the emulator is Stratus's problem, not the
//     function's, and becomes a ServiceException.
func (o rieOutcome) toInvokeResult(timeoutSeconds int) (lambdasvc.InvokeResult, error) {
	switch o.Status {
	case http.StatusOK:
		if isTimeoutBody(o.Body) {
			return lambdasvc.InvokeResult{}, errRuntimeTimedOut{payload: timeoutPayload(timeoutSeconds)}
		}
		if isErrorDocument(o.Body) {
			return lambdasvc.InvokeResult{Payload: o.Body, FunctionError: functionErrorUnhandled}, nil
		}
		return lambdasvc.InvokeResult{Payload: o.Body}, nil
	case http.StatusBadGateway:
		payload := o.Body
		if !isErrorDocument(payload) {
			payload = errorDocument("Runtime.Unknown", strings.TrimSpace(string(payload)))
		}
		return lambdasvc.InvokeResult{Payload: payload, FunctionError: functionErrorUnhandled}, nil
	default:
		return lambdasvc.InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    fmt.Sprintf("lambda runtime returned unexpected status %d: %s", o.Status, strings.TrimSpace(string(o.Body))),
		}
	}
}

func isTimeoutBody(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("Task timed out after"))
}

// isErrorDocument reports whether body is the error JSON runtimes post to
// the Runtime API: an object with errorType or errorMessage and no keys
// outside that document's shape.
func isErrorDocument(body []byte) bool {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || len(doc) == 0 {
		return false
	}
	_, hasType := doc["errorType"]
	_, hasMessage := doc["errorMessage"]
	if !hasType && !hasMessage {
		return false
	}
	for key := range doc {
		switch key {
		case "errorType", "errorMessage", "stackTrace", "requestId", "cause", "trace":
		default:
			return false
		}
	}
	return true
}

func errorDocument(errorType, message string) []byte {
	raw, _ := json.Marshal(map[string]string{"errorType": errorType, "errorMessage": message})
	return raw
}

func timeoutPayload(seconds int) []byte {
	if seconds <= 0 {
		seconds = defaultTimeoutS
	}
	return []byte(fmt.Sprintf(`{"errorMessage":"Task timed out after %d.00 seconds"}`, seconds))
}
