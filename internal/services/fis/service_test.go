package fis

import (
	"bytes"
	"encoding/json"
	"github.com/stratus/internal/scope"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/store/bbolt"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type fixture struct {
	svc    *Service
	engine *chaos.Engine
	clock  *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	c := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	engine := chaos.NewEngine([]string{"lambda", "dynamodb", "sqs", "sns", "events", "stepfunctions"}, c)
	svc := NewService(meta, engine)
	svc.now = c.Now
	return &fixture{svc: svc, engine: engine, clock: c}
}

// call sends a request through Handle and decodes the JSON response.
func (f *fixture) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	if err := f.svc.Handle(rec, req); err != nil {
		ae, ok := err.(*apierror.Error)
		if !ok {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return ae.StatusCode, map[string]any{"__type": ae.Code, "message": ae.Message}
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func obj(v any, keys ...string) map[string]any {
	m, _ := v.(map[string]any)
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}

var twoPhaseTemplate = map[string]any{
	"description": "slow, then failing, checkout",
	"targets": map[string]any{
		"checkout": map[string]any{"resourceType": "aws:lambda:function", "selectionMode": "ALL",
			"resourceArns": []string{"arn:aws:lambda:us-east-1:000000000000:function:checkout"}},
	},
	"actions": map[string]any{
		"slow": map[string]any{"actionId": "aws:lambda:invocation-add-delay", "targets": map[string]string{"Functions": "checkout"},
			"parameters": map[string]string{"duration": "PT1M", "startupDelayMilliseconds": "200"}},
		"fail": map[string]any{"actionId": "aws:lambda:invocation-error", "targets": map[string]string{"Functions": "checkout"},
			"parameters": map[string]string{"duration": "PT1M"}, "startAfter": []string{"slow"}},
	},
	"stopConditions": []map[string]string{{"source": "none"}},
	"roleArn":        "arn:aws:iam::000000000000:role/fis",
	"tags":           map[string]string{SeedTag: "1234"},
}

func (f *fixture) createTemplate(t *testing.T) string {
	t.Helper()
	status, out := f.call(t, http.MethodPost, "/experimentTemplates", twoPhaseTemplate)
	if status != http.StatusOK {
		t.Fatalf("create template: %d %v", status, out)
	}
	return obj(out, "experimentTemplate")["id"].(string)
}

func invoke(e *chaos.Engine) chaos.Decision {
	return e.Decide(chaos.Request{Service: "lambda", Operation: "Invoke", Function: "checkout"})
}

func TestTemplateCRUD(t *testing.T) {
	f := newFixture(t)
	id := f.createTemplate(t)

	status, out := f.call(t, http.MethodGet, "/experimentTemplates/"+id, nil)
	tmpl := obj(out, "experimentTemplate")
	if status != 200 || tmpl["description"] != "slow, then failing, checkout" || tmpl["arn"] == "" {
		t.Fatalf("get: %d %v", status, out)
	}
	if _, ok := tmpl["creationTime"].(float64); !ok {
		t.Fatalf("timestamps must be epoch seconds: %v", tmpl["creationTime"])
	}

	_, out = f.call(t, http.MethodPatch, "/experimentTemplates/"+id, map[string]any{"description": "renamed"})
	if obj(out, "experimentTemplate")["description"] != "renamed" || len(obj(out, "experimentTemplate", "actions")) != 2 {
		t.Fatalf("update should change only the description: %v", out)
	}

	_, out = f.call(t, http.MethodGet, "/experimentTemplates", nil)
	if list, _ := out["experimentTemplates"].([]any); len(list) != 1 {
		t.Fatalf("list: %v", out)
	}

	status, _ = f.call(t, http.MethodDelete, "/experimentTemplates/"+id, nil)
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, out = f.call(t, http.MethodGet, "/experimentTemplates/"+id, nil)
	if status != 404 || out["__type"] != "ResourceNotFoundException" {
		t.Fatalf("get deleted: %d %v", status, out)
	}
}

func TestInvalidTemplateIsRejectedAtCreate(t *testing.T) {
	f := newFixture(t)
	status, out := f.call(t, http.MethodPost, "/experimentTemplates", map[string]any{
		"actions": map[string]any{"x": map[string]any{"actionId": "aws:ec2:reboot-instances"}},
	})
	if status != 400 || out["__type"] != "ValidationException" {
		t.Fatalf("want ValidationException, got %d %v", status, out)
	}
	// Rules the chaos engine rejects fail at create time too.
	status, out = f.call(t, http.MethodPost, "/experimentTemplates", map[string]any{
		"actions": map[string]any{"x": map[string]any{"actionId": "aws:fis:inject-api-throttle-error",
			"parameters": map[string]string{"service": "nosuchservice"}}},
	})
	if status != 400 || out["__type"] != "ValidationException" {
		t.Fatalf("want ValidationException, got %d %v", status, out)
	}
}

func TestExperimentRunsActionsInSequenceThenCompletes(t *testing.T) {
	f := newFixture(t)
	id := f.createTemplate(t)

	status, out := f.call(t, http.MethodPost, "/experiments", map[string]any{"experimentTemplateId": id})
	exp := obj(out, "experiment")
	if status != 200 || obj(exp, "state")["status"] != StatusRunning {
		t.Fatalf("start: %d %v", status, out)
	}
	if exp["tags"].(map[string]any)[SeedTag] != "1234" {
		t.Fatalf("the template's seed should be used and reported: %v", exp["tags"])
	}
	expID := exp["id"].(string)
	if obj(exp, "actions", "slow", "state")["status"] != StatusRunning || obj(exp, "actions", "fail", "state")["status"] != StatusPending {
		t.Fatalf("action states at start: %v", exp["actions"])
	}

	// Phase 1: invocations are delayed.
	if d := invoke(f.engine); d.Delay != 200*time.Millisecond || d.FunctionError != "" {
		t.Fatalf("phase 1: %+v", d)
	}
	// Phase 2: invocations fail.
	f.clock.t = f.clock.t.Add(90 * time.Second)
	if d := invoke(f.engine); d.FunctionError == "" || d.Delay != 0 {
		t.Fatalf("phase 2: %+v", d)
	}
	_, out = f.call(t, http.MethodGet, "/experiments/"+expID, nil)
	if obj(out, "experiment", "actions", "slow", "state")["status"] != StatusCompleted ||
		obj(out, "experiment", "actions", "fail", "state")["status"] != StatusRunning {
		t.Fatalf("action states in phase 2: %v", obj(out, "experiment", "actions"))
	}

	// After both phases the experiment is complete and faults stop.
	f.clock.t = f.clock.t.Add(time.Minute)
	if d := invoke(f.engine); d.Faulted() {
		t.Fatalf("no faults after the experiment: %+v", d)
	}
	_, out = f.call(t, http.MethodGet, "/experiments/"+expID, nil)
	if obj(out, "experiment", "state")["status"] != StatusCompleted || obj(out, "experiment")["endTime"] == nil {
		t.Fatalf("want completed: %v", out)
	}
	status, out = f.call(t, http.MethodDelete, "/experiments/"+expID, nil)
	if status != 409 || out["__type"] != "ConflictException" {
		t.Fatalf("stopping a finished experiment: %d %v", status, out)
	}
}

func TestStopExperimentStopsFaults(t *testing.T) {
	f := newFixture(t)
	id := f.createTemplate(t)
	_, out := f.call(t, http.MethodPost, "/experiments", map[string]any{"experimentTemplateId": id, "tags": map[string]string{SeedTag: "7"}})
	exp := obj(out, "experiment")
	if exp["tags"].(map[string]any)[SeedTag] != "7" {
		t.Fatalf("StartExperiment tags override the template's seed: %v", exp["tags"])
	}

	status, out := f.call(t, http.MethodDelete, "/experiments/"+exp["id"].(string), nil)
	if status != 200 || obj(out, "experiment", "state")["status"] != StatusStopped {
		t.Fatalf("stop: %d %v", status, out)
	}
	if obj(out, "experiment", "actions", "fail", "state")["status"] != StatusCancelled {
		t.Fatalf("an action that never started is cancelled: %v", obj(out, "experiment", "actions"))
	}
	if d := invoke(f.engine); d.Faulted() {
		t.Fatalf("faults after stop: %+v", d)
	}
}

func TestReplacedExperimentReportsStopped(t *testing.T) {
	f := newFixture(t)
	id := f.createTemplate(t)
	_, out := f.call(t, http.MethodPost, "/experiments", map[string]any{"experimentTemplateId": id})
	first := obj(out, "experiment")["id"].(string)

	// A native chaos experiment replaces the FIS one.
	if _, err := f.engine.Start(chaos.Spec{Rules: []chaos.Rule{{Service: "sqs", Action: chaos.ActionDrop}}}); err != nil {
		t.Fatal(err)
	}
	_, out = f.call(t, http.MethodGet, "/experiments/"+first, nil)
	if obj(out, "experiment", "state")["status"] != StatusStopped {
		t.Fatalf("want stopped: %v", out)
	}
	// Stopping it must not stop the experiment that replaced it.
	f.call(t, http.MethodDelete, "/experiments/"+first, nil)
	if _, err := f.engine.Report(); err != nil {
		t.Fatalf("the replacing experiment should still run: %v", err)
	}

	_, out = f.call(t, http.MethodGet, "/experiments", nil)
	if list, _ := out["experiments"].([]any); len(list) != 1 {
		t.Fatalf("list: %v", out)
	}
}

func TestSameSeedReplaysSameFaults(t *testing.T) {
	run := func(seed int) []bool {
		f := newFixture(t)
		_, out := f.call(t, http.MethodPost, "/experimentTemplates", map[string]any{
			"actions": map[string]any{"flaky": map[string]any{"actionId": "stratus:delivery:drop",
				"parameters": map[string]string{"source": "sns", "service": "sqs", "percentage": "30"}}},
			"tags": map[string]string{SeedTag: strconv.Itoa(seed)},
		})
		id := obj(out, "experimentTemplate")["id"].(string)
		f.call(t, http.MethodPost, "/experiments", map[string]any{"experimentTemplateId": id})
		got := make([]bool, 50)
		for i := range got {
			got[i] = f.engine.DecideDelivery(chaos.Delivery{Source: "sns", Service: "sqs", Operation: "SendMessage"}).Drop
		}
		return got
	}
	a, b := run(5), run(5)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("delivery %d differs between runs with the same seed", i+1)
		}
	}
}

func TestActionsAndResourceTypes(t *testing.T) {
	f := newFixture(t)
	_, out := f.call(t, http.MethodGet, "/actions", nil)
	if list, _ := out["actions"].([]any); len(list) != len(newCatalog(scope.Default())) {
		t.Fatalf("list actions: %v", out)
	}
	status, out := f.call(t, http.MethodGet, "/actions/aws:lambda:invocation-error", nil)
	if status != 200 || obj(out, "action", "parameters", "invocationPercentage") == nil {
		t.Fatalf("get action: %d %v", status, out)
	}
	status, _ = f.call(t, http.MethodGet, "/actions/aws:ec2:stop-instances", nil)
	if status != 404 {
		t.Fatalf("unknown action: %d", status)
	}
	_, out = f.call(t, http.MethodGet, "/targetResourceTypes", nil)
	if list, _ := out["targetResourceTypes"].([]any); len(list) != 2 {
		t.Fatalf("resource types: %v", out)
	}
}

func TestOperationRouting(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"POST", "/experimentTemplates", "CreateExperimentTemplate"},
		{"PATCH", "/experimentTemplates/EXT1", "UpdateExperimentTemplate"},
		{"DELETE", "/experiments/EXP1", "StopExperiment"},
		{"GET", "/actions/aws%3Afis%3Await", "GetAction"},
		{"GET", "/targetResourceTypes", "ListTargetResourceTypes"},
		{"PUT", "/experiments", ""},
		{"GET", "/a/b/c", ""},
	}
	for _, tc := range cases {
		if got := Operation(tc.method, tc.path); got != tc.want {
			t.Errorf("Operation(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
