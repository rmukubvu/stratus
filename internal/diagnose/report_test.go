package diagnose

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func fail(id string, at time.Duration, service, op, code string, status int) Failure {
	return Failure{Time: t0.Add(at), RequestID: id, Service: service, Operation: op, Path: "/" + service, Status: status, Code: code, Message: code + " message"}
}

func TestExplainCategories(t *testing.T) {
	cases := []struct {
		code    string
		status  int
		want    Category
		yourBug bool
	}{
		{"NotImplementedException", 501, CategoryUnsupported, false},
		{"NotImplemented", 501, CategoryUnsupported, false},
		{"InvalidAction", 400, CategoryUnsupported, false},
		{"ResourceNotFoundException", 400, CategoryMissingResource, true},
		{"NoSuchBucket", 404, CategoryMissingResource, true},
		{"StateMachineDoesNotExist", 400, CategoryMissingResource, true},
		{"ResourceInUseException", 400, CategoryConflict, true},
		{"ParameterAlreadyExists", 400, CategoryConflict, true},
		{"BucketNotEmpty", 409, CategoryConflict, true},
		{"ValidationException", 400, CategoryInvalidRequest, true},
		{"InvalidParameterValue", 400, CategoryInvalidRequest, true},
		{"ThrottlingException", 400, CategoryThrottling, true},
		{"SlowDown", 503, CategoryThrottling, true},
		{"ProvisionedThroughputExceededException", 400, CategoryThrottling, true},
		{"NotAuthorizedException", 400, CategoryAuth, true},
		{"InvalidClientTokenId", 403, CategoryAuth, true},
		{"ConnectionDropped", 0, CategoryConnection, false},
		{"InternalFailure", 500, CategoryEmulator, false},
		{"SomethingOdd", 400, CategoryClient, true},
	}
	for _, tc := range cases {
		got := explain(failure{service: "dynamodb", operation: "PutItem", code: tc.code, status: tc.status, message: "m"})
		if got.Category != tc.want || got.YourBug != tc.yourBug {
			t.Errorf("%s: got %s/%v, want %s/%v", tc.code, got.Category, got.YourBug, tc.want, tc.yourBug)
		}
		if got.LikelyCause == "" || got.NextStep == "" {
			t.Errorf("%s: empty advice: %+v", tc.code, got)
		}
	}
}

func TestMissingResourceSuggestsListCommand(t *testing.T) {
	got := explain(failure{service: "sqs", operation: "SendMessage", code: "QueueDoesNotExist", status: 400})
	if !strings.Contains(got.NextStep, "aws sqs list-queues") {
		t.Fatalf("next step: %q", got.NextStep)
	}
}

func TestDiagnoseGroupsAndRanksRealProblemsFirst(t *testing.T) {
	failures := []Failure{
		fail("r1", 1*time.Second, "sqs", "SendMessage", "ThrottlingException", 400),
		fail("r2", 2*time.Second, "sqs", "SendMessage", "ThrottlingException", 400),
		fail("r3", 3*time.Second, "dynamodb", "GetItem", "ResourceNotFoundException", 400),
		fail("r4", 4*time.Second, "dynamodb", "GetItem", "ResourceNotFoundException", 400),
		fail("r5", 5*time.Second, "ecs", "RunTask", "NotImplementedException", 501),
	}
	injected := map[string]Injection{"r1": {RuleID: "throttle-sqs", Action: "throttle"}, "r2": {RuleID: "throttle-sqs", Action: "throttle"}}

	r := Diagnose(failures, injected, Options{})
	if r.Failures != 5 || r.Healthy || len(r.Groups) != 3 {
		t.Fatalf("report: %+v", r)
	}
	if r.Groups[0].Code != "NotImplementedException" || r.Groups[1].Code != "ResourceNotFoundException" {
		t.Fatalf("order: %s, %s", r.Groups[0].Code, r.Groups[1].Code)
	}
	last := r.Groups[2]
	if last.Category != CategoryInjected || last.Count != 2 || last.YourBug || !strings.Contains(last.LikelyCause, "throttle-sqs") {
		t.Fatalf("injected group: %+v", last)
	}
	ddb := r.Groups[1]
	if ddb.Count != 2 || !ddb.FirstSeen.Equal(t0.Add(3*time.Second)) || !ddb.LastSeen.Equal(t0.Add(4*time.Second)) || len(ddb.RequestIDs) != 2 {
		t.Fatalf("dynamodb group: %+v", ddb)
	}
	if r.ByCause[CategoryInjected] != 2 || r.ByCause[CategoryMissingResource] != 2 || r.ByCause[CategoryUnsupported] != 1 {
		t.Fatalf("by cause: %v", r.ByCause)
	}
	for _, want := range []string{"5 failed requests", "1 unsupported operation", "2 missing resource", "2 injected by chaos", "Start with ecs RunTask"} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("summary %q missing %q", r.Summary, want)
		}
	}
}

func TestDiagnoseIsHealthyWhenOnlyInjectedFailures(t *testing.T) {
	r := Diagnose([]Failure{fail("r1", 0, "s3", "PutObject", "SlowDown", 503)},
		map[string]Injection{"r1": {RuleID: "s3", Action: "throttle"}}, Options{})
	if !r.Healthy || !strings.Contains(r.Summary, "All failures were injected") {
		t.Fatalf("report: %+v", r)
	}
	if r := Diagnose(nil, nil, Options{}); !r.Healthy || r.Summary != "No failed requests." {
		t.Fatalf("empty: %+v", r)
	}
}

func TestDiagnoseFilters(t *testing.T) {
	failures := []Failure{
		fail("old", 0, "sqs", "SendMessage", "QueueDoesNotExist", 400),
		fail("new", time.Minute, "s3", "GetObject", "NoSuchKey", 404),
		fail("inj", 2*time.Minute, "s3", "PutObject", "SlowDown", 503),
	}
	injected := map[string]Injection{"inj": {RuleID: "x", Action: "throttle"}}

	r := Diagnose(failures, injected, Options{Since: t0.Add(30 * time.Second)})
	if r.Failures != 2 || r.Since == nil {
		t.Fatalf("since: %+v", r)
	}
	r = Diagnose(failures, injected, Options{Service: "sqs"})
	if r.Failures != 1 || r.Groups[0].Service != "sqs" {
		t.Fatalf("service: %+v", r)
	}
	r = Diagnose(failures, injected, Options{ExcludeInjected: true})
	if r.Failures != 2 || r.ByCause[CategoryInjected] != 0 {
		t.Fatalf("exclude injected: %+v", r)
	}
	r = Diagnose(failures, injected, Options{MaxGroups: 1})
	if len(r.Groups) != 1 || r.Failures != 3 {
		t.Fatalf("max groups: %+v", r)
	}
}

func TestExamplesAreCapped(t *testing.T) {
	var failures []Failure
	for i := 0; i < 10; i++ {
		f := fail("r", time.Duration(i)*time.Second, "s3", "GetObject", "NoSuchKey", 404)
		f.RequestID = f.RequestID + string(rune('a'+i))
		failures = append(failures, f)
	}
	g := Diagnose(failures, nil, Options{}).Groups[0]
	if g.Count != 10 || len(g.RequestIDs) != maxExamples || len(g.Paths) != 1 {
		t.Fatalf("group: %+v", g)
	}
}

func TestParseSince(t *testing.T) {
	if got, err := ParseSince("5m", t0); err != nil || !got.Equal(t0.Add(-5*time.Minute)) {
		t.Fatalf("duration: %v %v", got, err)
	}
	if got, err := ParseSince("2026-11-05T09:00:00Z", t0); err != nil || !got.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("rfc3339: %v %v", got, err)
	}
	for _, bad := range []string{"yesterday", "-5m", "0s"} {
		if _, err := ParseSince(bad, t0); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
