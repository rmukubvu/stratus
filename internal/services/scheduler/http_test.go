package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOperationRoutesByMethodAndPath(t *testing.T) {
	cases := map[string]string{
		"POST /schedules/nightly":               "CreateSchedule",
		"GET /schedules/nightly":                "GetSchedule",
		"PUT /schedules/nightly":                "UpdateSchedule",
		"DELETE /schedules/nightly?groupName=g": "DeleteSchedule",
		"GET /schedules":                        "ListSchedules",
		"GET /schedules/":                       "ListSchedules",
		"POST /schedule-groups/billing":         "CreateScheduleGroup",
		"GET /schedule-groups/billing":          "GetScheduleGroup",
		"DELETE /schedule-groups/billing":       "DeleteScheduleGroup",
		"GET /schedule-groups":                  "ListScheduleGroups",
		"GET /tags/arn:aws:scheduler:us-east-1:000000000000:schedule-group/billing":        "ListTagsForResource",
		"POST /tags/arn%3Aaws%3Ascheduler%3Aus-east-1%3A000000000000%3Aschedule-group%2Fx": "TagResource",
		"DELETE /tags/arn:aws:scheduler:us-east-1:000000000000:schedule-group/billing":     "UntagResource",
		"PATCH /schedules/nightly": "",
		"GET /schedules/a/b":       "",
		"POST /schedules":          "",
		"GET /experiments":         "",
	}
	for request, want := range cases {
		method, rawPath, _ := strings.Cut(request, " ")
		path, _, _ := strings.Cut(rawPath, "?")
		req := httptest.NewRequest(method, rawPath, nil)
		if got := Operation(method, req.URL.Path); got != want {
			t.Errorf("%s: got %q, want %q (path %s)", request, got, want, path)
		}
	}
}

func call(t *testing.T, svc *Service, method, path string, payload any) (int, map[string]any, error) {
	t.Helper()
	var body string
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = string(raw)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, req)
	out := map[string]any{}
	if err == nil {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, err
}

func TestRESTScheduleLifecycle(t *testing.T) {
	svc, _, _ := newTestService(t)

	start := float64(t0.Add(time.Hour).Unix())
	status, out, err := call(t, svc, http.MethodPost, "/schedules/nightly", map[string]any{
		"ScheduleExpression":         "cron(0 2 * * ? *)",
		"ScheduleExpressionTimezone": "Europe/London",
		"FlexibleTimeWindow":         map[string]any{"Mode": "OFF"},
		"StartDate":                  start,
		"Description":                "nightly rollup",
		"Target": map[string]any{
			"Arn":     queueARN,
			"RoleArn": "arn:aws:iam::000000000000:role/scheduler",
			"Input":   `{"kind":"rollup"}`,
			"SqsParameters": map[string]any{
				"MessageGroupId": "rollups",
			},
		},
	})
	if err != nil || status != http.StatusOK || out["ScheduleArn"] != "arn:aws:scheduler:us-east-1:000000000000:schedule/default/nightly" {
		t.Fatalf("create: %d %v %v", status, out, err)
	}

	_, out, err = call(t, svc, http.MethodGet, "/schedules/nightly?groupName=default", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out["Name"] != "nightly" || out["State"] != "ENABLED" || out["ScheduleExpressionTimezone"] != "Europe/London" || out["StartDate"] != start || out["Description"] != "nightly rollup" {
		t.Fatalf("get: %v", out)
	}
	if created, _ := out["CreationDate"].(float64); created != float64(t0.Unix()) {
		t.Fatalf("dates are epoch seconds: %v", out["CreationDate"])
	}
	target, _ := out["Target"].(map[string]any)
	if target["Arn"] != queueARN || target["SqsParameters"].(map[string]any)["MessageGroupId"] != "rollups" || target["RetryPolicy"] == nil {
		t.Fatalf("target echoed with defaults: %v", target)
	}

	_, out, err = call(t, svc, http.MethodPut, "/schedules/nightly", map[string]any{
		"ScheduleExpression": "rate(2 hours)",
		"FlexibleTimeWindow": map[string]any{"Mode": "FLEXIBLE", "MaximumWindowInMinutes": 15},
		"State":              "DISABLED",
		"Target":             map[string]any{"Arn": queueARN},
	})
	if err != nil || out["ScheduleArn"] == nil {
		t.Fatalf("update: %v %v", out, err)
	}
	_, out, _ = call(t, svc, http.MethodGet, "/schedules/nightly", nil)
	if out["State"] != "DISABLED" || out["ScheduleExpression"] != "rate(2 hours)" || out["StartDate"] != nil {
		t.Fatalf("update replaces the definition: %v", out)
	}

	_, _, _ = call(t, svc, http.MethodPost, "/schedules/other", map[string]any{"ScheduleExpression": "rate(1 day)", "FlexibleTimeWindow": map[string]any{"Mode": "OFF"}, "Target": map[string]any{"Arn": queueARN}})
	_, out, _ = call(t, svc, http.MethodGet, "/schedules?State=DISABLED", nil)
	items, _ := out["Schedules"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["Name"] != "nightly" || items[0].(map[string]any)["Target"].(map[string]any)["Arn"] != queueARN {
		t.Fatalf("list filter: %v", out)
	}
	_, out, _ = call(t, svc, http.MethodGet, "/schedules?NamePrefix=o", nil)
	if items, _ := out["Schedules"].([]any); len(items) != 1 {
		t.Fatalf("name prefix: %v", out)
	}

	if _, _, err := call(t, svc, http.MethodDelete, "/schedules/nightly", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, _, err = call(t, svc, http.MethodGet, "/schedules/nightly", nil)
	requireCode(t, err, "ResourceNotFoundException")
	_, _, err = call(t, svc, http.MethodPost, "/schedules/bad", map[string]any{"ScheduleExpression": "rate(1 day)", "FlexibleTimeWindow": map[string]any{"Mode": "OFF"}, "Target": map[string]any{"Arn": "arn:aws:kinesis:us-east-1:000000000000:stream/s"}})
	requireCode(t, err, "ValidationException")
	_, _, err = call(t, svc, http.MethodPatch, "/schedules/bad", nil)
	requireCode(t, err, "ResourceNotFoundException")

	req := httptest.NewRequest(http.MethodPost, "/schedules/x", strings.NewReader("{not json"))
	requireCode(t, svc.Handle(httptest.NewRecorder(), req), "ValidationException")
}

func TestRESTAcceptsRFC3339DatesToo(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _, err := call(t, svc, http.MethodPost, "/schedules/later", map[string]any{
		"ScheduleExpression": "rate(1 hour)",
		"FlexibleTimeWindow": map[string]any{"Mode": "OFF"},
		"StartDate":          "2026-11-09T15:00:00Z",
		"Target":             map[string]any{"Arn": queueARN},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sch, _ := svc.GetSchedule("", "later")
	if !sch.StartDate.Equal(t0.Add(3*time.Hour)) || !sch.NextInvocation.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("start date: %v next %v", sch.StartDate, sch.NextInvocation)
	}
}

func TestRESTGroupsAndTags(t *testing.T) {
	svc, _, _ := newTestService(t)
	groupArn := "arn:aws:scheduler:us-east-1:000000000000:schedule-group/billing"

	_, out, err := call(t, svc, http.MethodPost, "/schedule-groups/billing", map[string]any{"Tags": []map[string]string{{"Key": "team", "Value": "payments"}}})
	if err != nil || out["ScheduleGroupArn"] != groupArn {
		t.Fatalf("create group: %v %v", out, err)
	}
	_, out, _ = call(t, svc, http.MethodGet, "/schedule-groups/billing", nil)
	if out["Name"] != "billing" || out["State"] != "ACTIVE" || out["Arn"] != groupArn {
		t.Fatalf("get group: %v", out)
	}
	_, out, _ = call(t, svc, http.MethodGet, "/schedule-groups", nil)
	if items, _ := out["ScheduleGroups"].([]any); len(items) != 2 || items[0].(map[string]any)["Name"] != "billing" || items[1].(map[string]any)["Name"] != "default" {
		t.Fatalf("list groups includes default: %v", out)
	}
	_, out, _ = call(t, svc, http.MethodGet, "/schedule-groups?NamePrefix=bill", nil)
	if items, _ := out["ScheduleGroups"].([]any); len(items) != 1 {
		t.Fatalf("prefix filter: %v", out)
	}

	escaped := "/tags/" + strings.NewReplacer(":", "%3A", "/", "%2F").Replace(groupArn)
	if _, _, err := call(t, svc, http.MethodPost, escaped, map[string]any{"Tags": []map[string]string{{"Key": "env", "Value": "ci"}}}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, _, err := call(t, svc, http.MethodDelete, escaped+"?TagKeys=team", nil); err != nil {
		t.Fatalf("untag: %v", err)
	}
	_, out, _ = call(t, svc, http.MethodGet, escaped, nil)
	tags, _ := out["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "env" {
		t.Fatalf("tags: %v", out)
	}
	_, _, err = call(t, svc, http.MethodGet, "/tags/arn:aws:scheduler:us-east-1:000000000000:schedule-group/nope", nil)
	requireCode(t, err, "ResourceNotFoundException")

	_, _, err = call(t, svc, http.MethodDelete, "/schedule-groups/default", nil)
	requireCode(t, err, "ValidationException")
	if _, _, err := call(t, svc, http.MethodDelete, "/schedule-groups/billing", nil); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	_, _, err = call(t, svc, http.MethodGet, "/schedule-groups/billing", nil)
	requireCode(t, err, "ResourceNotFoundException")
}

// muxRouter adapts the standard mux to the control API's Router shape.
type muxRouter struct{ *http.ServeMux }

func (m muxRouter) HandleFunc(pattern string, fn http.HandlerFunc) {
	m.ServeMux.HandleFunc(pattern, fn)
}

func TestControlRoutesDriveTheClock(t *testing.T) {
	svc, _, gate := newTestService(t)
	mux := http.NewServeMux()
	RegisterRoutes(muxRouter{mux}, svc)
	if _, err := svc.CreateSchedule(input("job", "rate(10 minutes)")); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	base := "/_stratus/control/v1/scheduler"

	status, out := do(http.MethodGet, base+"/clock", "")
	if status != http.StatusOK || out["offset"] != "0s" {
		t.Fatalf("clock: %d %v", status, out)
	}
	status, out = do(http.MethodPost, base+"/advance", `{"by":"15m"}`)
	fired, _ := out["fired"].([]any)
	if status != http.StatusOK || out["offset"] != "15m0s" || len(fired) != 1 || fired[0].(map[string]any)["name"] != "job" {
		t.Fatalf("advance: %d %v", status, out)
	}
	status, out = do(http.MethodPost, base+"/advance", `{"to":"2026-11-09T13:00:00Z"}`)
	if status != http.StatusOK || out["offset"] != "1h0m0s" {
		t.Fatalf("advance to: %d %v", status, out)
	}
	for _, bad := range []string{`{}`, `{"by":"soon"}`, `{"to":"noon"}`, `{"by":"1m","to":"2026-11-09T14:00:00Z"}`, `{"by":"-5m"}`, `{"to":"2020-01-01T00:00:00Z"}`} {
		if status, out := do(http.MethodPost, base+"/advance", bad); status != http.StatusBadRequest || out["error"] == nil {
			t.Errorf("%s should be a 400 control error, got %d %v", bad, status, out)
		}
	}

	status, out = do(http.MethodPost, base+"/run", `{"name":"job"}`)
	if status != http.StatusOK || out["name"] != "job" || out["execution_id"] == "" || out["scheduled_time"] != "2026-11-09T13:00:00Z" {
		t.Fatalf("run: %d %v", status, out)
	}
	status, out = do(http.MethodPost, base+"/run", `{"name":"missing","group":"default"}`)
	if status != http.StatusNotFound || out["error"].(map[string]any)["code"] != "ResourceNotFoundException" {
		t.Fatalf("run missing: %d %v", status, out)
	}
	if status, _ := do(http.MethodPost, base+"/run", `{}`); status != http.StatusBadRequest {
		t.Fatalf("run without a name: %d", status)
	}
	if gate.count() != 3 {
		t.Fatalf("two advances and one run deliver three times, got %d", gate.count())
	}
}
