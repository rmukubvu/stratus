package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
)

func TestEC2ErrorsUseTheEC2Envelope(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(WithRequestMetadata(r.Context(), RequestMetadata{
		RequestID:      "req-1",
		Classification: Classification{Protocol: ProtocolQuery, Service: "ec2", Operation: "DeleteVpc"},
	}))
	w := httptest.NewRecorder()
	WriteError(w, r, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "DependencyViolation", Message: "busy"})

	want := `<Response><Errors><Error><Code>DependencyViolation</Code><Message>busy</Message></Error></Errors><RequestID>req-1</RequestID></Response>`
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestOtherQueryErrorsKeepTheErrorResponseEnvelope(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(WithRequestMetadata(r.Context(), RequestMetadata{
		RequestID:      "req-2",
		Classification: Classification{Protocol: ProtocolQuery, Service: "sns", Operation: "Publish"},
	}))
	w := httptest.NewRecorder()
	WriteError(w, r, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFound", Message: "no topic"})
	if !strings.Contains(w.Body.String(), "<ErrorResponse><Error><Code>NotFound</Code>") {
		t.Fatalf("%s", w.Body.String())
	}
}
