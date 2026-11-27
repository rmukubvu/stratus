package events

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store/bbolt"
)

func newFixture(t *testing.T) func(op, body string) (map[string]any, error) {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	// Built directly rather than with NewService, which starts the scheduler.
	svc := &Service{scope: scope.Default(), metadata: meta, faults: chaos.Passthrough{}, now: time.Now}
	return func(op, body string) (map[string]any, error) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		if err := svc.Handle(w, req, op); err != nil {
			return nil, err
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out, nil
	}
}

func apiCode(err error) string {
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func TestDescribeEventBusFindsCustomAndDefaultBuses(t *testing.T) {
	call := newFixture(t)
	if _, err := call("CreateEventBus", `{"Name":"orders"}`); err != nil {
		t.Fatal(err)
	}
	out, err := call("DescribeEventBus", `{"Name":"orders"}`)
	if err != nil || out["Arn"] != "arn:aws:events:us-east-1:000000000000:event-bus/orders" {
		t.Fatalf("custom bus: %v, %v", out, err)
	}
	if out, err := call("DescribeEventBus", `{}`); err != nil || out["Name"] != "default" {
		t.Errorf("default bus: %v, %v", out, err)
	}
	if _, err := call("DescribeEventBus", `{"Name":"missing"}`); apiCode(err) != "ResourceNotFoundException" {
		t.Errorf("missing bus: %v", err)
	}
}

func tagsOf(t *testing.T, call func(string, string) (map[string]any, error), arn string) string {
	t.Helper()
	out, err := call("ListTagsForResource", `{"ResourceARN":"`+arn+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out["Tags"])
	return string(raw)
}

func TestRulesAndBusesKeepTheirTags(t *testing.T) {
	call := newFixture(t)
	if _, err := call("CreateEventBus", `{"Name":"orders","Tags":[{"Key":"team","Value":"payments"}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("PutRule", `{"Name":"paid","EventBusName":"orders","EventPattern":"{\"source\":[\"shop\"]}","Tags":[{"Key":"env","Value":"dev"}]}`); err != nil {
		t.Fatal(err)
	}
	bus := "arn:aws:events:us-east-1:000000000000:event-bus/orders"
	rule := "arn:aws:events:us-east-1:000000000000:rule/orders/paid"
	if got := tagsOf(t, call, bus); got != `[{"Key":"team","Value":"payments"}]` {
		t.Errorf("bus tags = %s", got)
	}
	if _, err := call("TagResource", `{"ResourceARN":"`+rule+`","Tags":[{"Key":"owner","Value":"ana"}]}`); err != nil {
		t.Fatal(err)
	}
	// Updating the rule keeps its tags.
	if _, err := call("PutRule", `{"Name":"paid","EventBusName":"orders","EventPattern":"{\"source\":[\"shop2\"]}"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("UntagResource", `{"ResourceARN":"`+rule+`","TagKeys":["env"]}`); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, call, rule); got != `[{"Key":"owner","Value":"ana"}]` {
		t.Errorf("rule tags = %s", got)
	}
	if _, err := call("ListTagsForResource", `{"ResourceARN":"arn:aws:events:us-east-1:000000000000:rule/nope"}`); apiCode(err) != "ResourceNotFoundException" {
		t.Errorf("missing rule: %v", err)
	}
	if _, err := call("ListTagsForResource", `{"ResourceARN":"not-an-arn"}`); apiCode(err) != "ValidationException" {
		t.Errorf("bad ARN: %v", err)
	}
}
