package container

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
)

func TestRIEOutcomeSuccess(t *testing.T) {
	got, err := rieOutcome{Status: http.StatusOK, Body: []byte(`{"count":1}`)}.toInvokeResult(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionError != "" || string(got.Payload) != `{"count":1}` {
		t.Fatalf("result = %+v", got)
	}
}

func TestRIEOutcomeHandlerErrorOn502(t *testing.T) {
	body := `{"errorMessage":"boom","errorType":"ValueError","stackTrace":["x"],"requestId":"r"}`
	got, err := rieOutcome{Status: http.StatusBadGateway, Body: []byte(body)}.toInvokeResult(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionError != "Unhandled" || string(got.Payload) != body {
		t.Fatalf("result = %+v", got)
	}
}

func TestRIEOutcomeRuntimeExitWithoutDocumentIsWrapped(t *testing.T) {
	got, err := rieOutcome{Status: http.StatusBadGateway, Body: []byte("exit status 1")}.toInvokeResult(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionError != "Unhandled" || !strings.Contains(string(got.Payload), `"errorMessage":"exit status 1"`) {
		t.Fatalf("result = %+v (%s)", got, got.Payload)
	}
}

func TestRIEOutcomeErrorDocumentOn200IsAFunctionError(t *testing.T) {
	got, err := rieOutcome{Status: http.StatusOK, Body: []byte(`{"errorType":"Runtime.ExitError","errorMessage":"exit"}`)}.toInvokeResult(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionError != "Unhandled" {
		t.Fatalf("FunctionError = %q", got.FunctionError)
	}
}

func TestRIEOutcomeOrdinaryObjectWithErrorFieldIsNotAnError(t *testing.T) {
	got, err := rieOutcome{Status: http.StatusOK, Body: []byte(`{"errorMessage":"none","items":[]}`)}.toInvokeResult(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionError != "" {
		t.Fatalf("FunctionError = %q, want none", got.FunctionError)
	}
}

func TestRIEOutcomeTimeout(t *testing.T) {
	_, err := rieOutcome{Status: http.StatusOK, Body: []byte("Task timed out after 1.00 seconds")}.toInvokeResult(1)
	var timedOut errRuntimeTimedOut
	if !errors.As(err, &timedOut) {
		t.Fatalf("error = %v, want errRuntimeTimedOut", err)
	}
	if !strings.Contains(string(timedOut.payload), "Task timed out after 1.00 seconds") {
		t.Fatalf("payload = %s", timedOut.payload)
	}
}

func TestRIEOutcomeUnexpectedStatusIsServiceException(t *testing.T) {
	_, err := rieOutcome{Status: http.StatusBadRequest, Body: []byte("already reserved")}.toInvokeResult(3)
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "ServiceException" {
		t.Fatalf("error = %v, want ServiceException", err)
	}
}

func TestInvokePayloadDefaultsToAnEmptyObject(t *testing.T) {
	for in, want := range map[string]string{"": "{}", "  \n": "{}", `{"a":1}`: `{"a":1}`, `"text"`: `"text"`} {
		if got := string(invokePayload([]byte(in))); got != want {
			t.Errorf("invokePayload(%q) = %q, want %q", in, got, want)
		}
	}
}
