package cloudformation

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

const emptyTemplate = `{"Resources":{}}`

func call(t *testing.T, svc *Service, action string, params ...string) (string, error) {
	t.Helper()
	form := url.Values{"Action": {action}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Set(params[i], params[i+1])
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if err := svc.Handle(w, req, action, "req"); err != nil {
		return "", err
	}
	return w.Body.String(), nil
}

func newStackService(t *testing.T) *Service {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	return NewService(Options{Metadata: meta})
}

func TestStackTagsAreStoredAndDescribed(t *testing.T) {
	svc := newStackService(t)
	if _, err := call(t, svc, "CreateStack", "StackName", "app", "TemplateBody", emptyTemplate,
		"Tags.member.1.Key", "env", "Tags.member.1.Value", "dev"); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, svc, "DescribeStacks", "StackName", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<Tags><member><Key>env</Key><Value>dev</Value></member></Tags>") {
		t.Fatalf("DescribeStacks: %s", out)
	}
}

func TestStackTagsAreValidated(t *testing.T) {
	svc := newStackService(t)
	_, err := call(t, svc, "CreateStack", "StackName", "app", "TemplateBody", emptyTemplate,
		"Tags.member.1.Key", "aws:reserved", "Tags.member.1.Value", "x")
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "ValidationError" {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}
