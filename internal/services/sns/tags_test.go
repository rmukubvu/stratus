package sns

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

func newTagFixture(t *testing.T) func(action string, params ...string) (string, error) {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	svc := NewService(meta)
	return func(action string, params ...string) (string, error) {
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
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestTopicTagsRoundTrip(t *testing.T) {
	call := newTagFixture(t)
	out, err := call("CreateTopic", "Name", "orders", "Tags.member.1.Key", "env", "Tags.member.1.Value", "dev")
	if err != nil {
		t.Fatal(err)
	}
	arn := between(out, "<TopicArn>", "</TopicArn>")

	if _, err := call("TagResource", "ResourceArn", arn, "Tags.member.1.Key", "team", "Tags.member.1.Value", "core"); err != nil {
		t.Fatal(err)
	}
	if _, err := call("UntagResource", "ResourceArn", arn, "TagKeys.member.1", "env"); err != nil {
		t.Fatal(err)
	}
	out, err = call("ListTagsForResource", "ResourceArn", arn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<Tags><member><Key>team</Key><Value>core</Value></member></Tags>") {
		t.Fatalf("ListTagsForResource: %s", out)
	}
}

func TestCreateTopicIsIdempotentOnlyWithTheSameTags(t *testing.T) {
	call := newTagFixture(t)
	tagged := []string{"Name", "orders", "Tags.member.1.Key", "env", "Tags.member.1.Value", "dev"}
	if _, err := call("CreateTopic", tagged...); err != nil {
		t.Fatal(err)
	}
	if _, err := call("CreateTopic", tagged...); err != nil {
		t.Fatalf("same tags should return the existing topic: %v", err)
	}
	if _, err := call("CreateTopic", "Name", "orders"); err != nil {
		t.Fatalf("no tags should return the existing topic: %v", err)
	}
	_, err := call("CreateTopic", "Name", "orders", "Tags.member.1.Key", "env", "Tags.member.1.Value", "prod")
	wantCode(t, err, "InvalidParameter")
}

func TestTopicTagErrors(t *testing.T) {
	call := newTagFixture(t)
	_, err := call("ListTagsForResource", "ResourceArn", "arn:aws:sns:us-east-1:000000000000:missing")
	wantCode(t, err, "ResourceNotFound")
	_, err = call("CreateTopic", "Name", "dup", "Tags.member.1.Key", "k", "Tags.member.2.Key", "k")
	wantCode(t, err, "InvalidParameter")
}

func between(s, open, close string) string {
	_, rest, _ := strings.Cut(s, open)
	v, _, _ := strings.Cut(rest, close)
	return v
}
