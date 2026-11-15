package lambda

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FunctionError is the error an event delivery returns when the function
// ran and failed: it threw, timed out or exited. Nobody waits on an event
// delivery, so without it the failure would leave no trace.
type FunctionError struct {
	Function string
	// Kind is Lambda's X-Amz-Function-Error value, "Unhandled" or "Handled".
	Kind string
	// Type and Message come from the function's error document.
	Type    string
	Message string
}

func (e *FunctionError) Error() string {
	detail := e.Message
	if e.Type != "" {
		detail = e.Type + ": " + e.Message
	}
	return fmt.Sprintf("function %s failed (%s): %s", e.Function, e.Kind, detail)
}

// Failure returns a *FunctionError when the invocation failed inside the
// function, and nil when it succeeded.
func (r InvokeResult) Failure(function string) error {
	if r.FunctionError == "" {
		return nil
	}
	var doc struct {
		ErrorType    string `json:"errorType"`
		ErrorMessage string `json:"errorMessage"`
	}
	message := strings.TrimSpace(string(r.Payload))
	if json.Unmarshal(r.Payload, &doc) == nil && (doc.ErrorType != "" || doc.ErrorMessage != "") {
		message = doc.ErrorMessage
	}
	return &FunctionError{Function: function, Kind: r.FunctionError, Type: doc.ErrorType, Message: message}
}
