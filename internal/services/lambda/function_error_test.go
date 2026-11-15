package lambda

import (
	"errors"
	"strings"
	"testing"
)

func TestFailureIsNilForASuccessfulInvocation(t *testing.T) {
	if err := (InvokeResult{Payload: []byte(`{"ok":true}`)}).Failure("fn"); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestFailureReadsTheFunctionsErrorDocument(t *testing.T) {
	result := InvokeResult{FunctionError: "Unhandled", Payload: []byte(`{"errorType":"TypeError","errorMessage":"Float types are not supported","stackTrace":[]}`)}
	var fe *FunctionError
	if !errors.As(result.Failure("process-order"), &fe) {
		t.Fatal("want a *FunctionError")
	}
	if fe.Function != "process-order" || fe.Kind != "Unhandled" || fe.Type != "TypeError" || fe.Message != "Float types are not supported" {
		t.Fatalf("got %+v", fe)
	}
	if !strings.Contains(fe.Error(), "TypeError: Float types are not supported") {
		t.Fatalf("message: %q", fe.Error())
	}
}

func TestFailureKeepsAPayloadThatIsNotAnErrorDocument(t *testing.T) {
	var fe *FunctionError
	if !errors.As((InvokeResult{FunctionError: "Unhandled", Payload: []byte("Task timed out")}).Failure("fn"), &fe) || fe.Message != "Task timed out" {
		t.Fatalf("got %+v", fe)
	}
}
