package validation

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
)

func TestAnInvalidRequestGetsTheServicesValidationError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"logGroupName": ""}`))
	err := New().Validate(r, "logs", "CreateLogGroup")
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("Validate = %v, want an AWS error", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "InvalidParameterException" {
		t.Fatalf("got %d %s", apiErr.StatusCode, apiErr.Code)
	}
	if !strings.HasPrefix(apiErr.Message, "2 validation errors detected: Value '' at 'logGroupName' failed to satisfy constraint: Member must have length greater than or equal to 1; ") {
		t.Fatalf("message %q", apiErr.Message)
	}
}

func TestAValidRequestReachesTheServiceWithItsBodyIntact(t *testing.T) {
	body := `{"TableName": "orders"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if err := New().Validate(r, "dynamodb", "DescribeTable"); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	if string(got) != body {
		t.Fatalf("body after validation = %q", got)
	}
}

func TestRequestsValidationCannotJudgePassThrough(t *testing.T) {
	cases := map[string]struct{ service, operation, body string }{
		"service not validated": {"sqs", "SendMessage", `{}`},
		"unknown operation":     {"dynamodb", "DescribeTables", `{}`},
		"body not JSON":         {"dynamodb", "DescribeTable", `not json`},
	}
	for name, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
		if err := New().Validate(r, c.service, c.operation); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
}
