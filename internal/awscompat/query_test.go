package awscompat

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseQueryFormLeavesTheBodyReadable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/?Version=2012-11-05", strings.NewReader("Action=ListQueues&QueueNamePrefix=a"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	form, err := ParseQueryForm(r)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if form.Get("Action") != "ListQueues" || form.Get("Version") != "2012-11-05" || r.PostForm.Get("QueueNamePrefix") != "a" {
		t.Fatalf("form = %v, post form = %v", form, r.PostForm)
	}
	body, _ := io.ReadAll(r.Body)
	if string(body) != "Action=ListQueues&QueueNamePrefix=a" {
		t.Fatalf("body after parsing = %q; signature verification needs the raw bytes", body)
	}
	if r.FormValue("Action") != "ListQueues" {
		t.Fatal("net/http form helpers should see the cached form")
	}
}

func TestParseQueryFormWithoutContentType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
	form, err := ParseQueryForm(r)
	if err != nil || form.Get("Action") != "GetCallerIdentity" {
		t.Fatalf("form = %v, err = %v", form, err)
	}
}

func TestParseQueryFormIgnoresANonFormBodyWhenTheQueryHasParameters(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/?Action=Publish", strings.NewReader(`{"a":"100%"}`))
	form, err := ParseQueryForm(r)
	if err != nil || form.Get("Action") != "Publish" {
		t.Fatalf("form = %v, err = %v", form, err)
	}
}
