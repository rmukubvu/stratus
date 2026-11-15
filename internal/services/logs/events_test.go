package logs

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stratus/internal/store/bbolt"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return NewService(metadata)
}

func call(t *testing.T, svc *Service, operation string, payload any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, httptest.NewRequest("POST", "http://localhost/", bytes.NewReader(body)), operation); err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s response: %v", operation, err)
	}
	return out
}

func messages(t *testing.T, resp map[string]any) []string {
	t.Helper()
	var out []string
	for _, e := range resp["events"].([]any) {
		out = append(out, e.(map[string]any)["message"].(string))
	}
	return out
}

var at = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func TestAppendCreatesTheGroupAndStreamAndKeepsLineOrder(t *testing.T) {
	svc := newTestService(t)
	lines := []string{"START RequestId: 1", "failed m-1: Float types are not supported", "END RequestId: 1"}
	if err := svc.Append("/aws/lambda/process-order", "2026/11/15/[$LATEST]stratus", lines, at); err != nil {
		t.Fatal(err)
	}
	resp := call(t, svc, "GetLogEvents", map[string]any{"logGroupName": "/aws/lambda/process-order", "logStreamName": "2026/11/15/[$LATEST]stratus", "startFromHead": true})
	got := messages(t, resp)
	if len(got) != 3 || got[0] != lines[0] || got[1] != lines[1] || got[2] != lines[2] {
		t.Fatalf("events in the same millisecond lost their order: %v", got)
	}
}

func TestGetLogEventsForwardTokenReturnsOnlyNewEvents(t *testing.T) {
	svc := newTestService(t)
	group, stream := "/aws/lambda/fn", "s"
	_ = svc.Append(group, stream, []string{"a", "b"}, at)
	first := call(t, svc, "GetLogEvents", map[string]any{"logGroupName": group, "logStreamName": stream})
	token := first["nextForwardToken"].(string)

	again := call(t, svc, "GetLogEvents", map[string]any{"logGroupName": group, "logStreamName": stream, "nextToken": token})
	if len(messages(t, again)) != 0 || again["nextForwardToken"] != token {
		t.Fatalf("with nothing new the token must repeat (that ends SDK paginators): %+v", again)
	}

	_ = svc.Append(group, stream, []string{"c"}, at.Add(time.Second))
	more := call(t, svc, "GetLogEvents", map[string]any{"logGroupName": group, "logStreamName": stream, "nextToken": token})
	if got := messages(t, more); len(got) != 1 || got[0] != "c" {
		t.Fatalf("following the token: %v", got)
	}
}

func TestFilterLogEventsMatchesTermsAcrossStreams(t *testing.T) {
	svc := newTestService(t)
	group := "/aws/lambda/fn"
	_ = svc.Append(group, "one", []string{"order o-1 stored", "order o-2 failed: TypeError"}, at)
	_ = svc.Append(group, "two", []string{"order o-3 failed: timeout"}, at.Add(time.Second))

	resp := call(t, svc, "FilterLogEvents", map[string]any{"logGroupName": group, "filterPattern": "failed"})
	if got := messages(t, resp); len(got) != 2 || got[0] != "order o-2 failed: TypeError" || got[1] != "order o-3 failed: timeout" {
		t.Fatalf("filter: %v", got)
	}
	phrase := call(t, svc, "FilterLogEvents", map[string]any{"logGroupName": group, "filterPattern": `"failed: TypeError"`})
	if got := messages(t, phrase); len(got) != 1 {
		t.Fatalf("phrase: %v", got)
	}
	window := call(t, svc, "FilterLogEvents", map[string]any{"logGroupName": group, "startTime": at.Add(time.Second).UnixMilli()})
	if got := messages(t, window); len(got) != 1 || got[0] != "order o-3 failed: timeout" {
		t.Fatalf("start time: %v", got)
	}
	if _, more := resp["nextToken"]; more {
		t.Fatal("a complete result must not carry a nextToken")
	}
}

func TestFilterTerms(t *testing.T) {
	got := filterTerms(`ERROR "Task timed out" order`)
	if len(got) != 3 || got[0] != "ERROR" || got[1] != "Task timed out" || got[2] != "order" {
		t.Fatalf("terms: %q", got)
	}
}
