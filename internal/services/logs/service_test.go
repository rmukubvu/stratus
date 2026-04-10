package logs

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

func TestLogsLifecycleAndOperatorViews(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)

	mustHandle := func(operation string, payload any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewReader(body))
		if err := svc.Handle(rec, req, operation); err != nil {
			t.Fatalf("%s failed: %v", operation, err)
		}
	}

	mustHandle("CreateLogGroup", map[string]any{
		"logGroupName": "/aws/lambda/test-func",
	})
	mustHandle("CreateLogStream", map[string]any{
		"logGroupName":  "/aws/lambda/test-func",
		"logStreamName": "2026/04/10/[$LATEST]abcdef",
	})
	mustHandle("PutLogEvents", map[string]any{
		"logGroupName":  "/aws/lambda/test-func",
		"logStreamName": "2026/04/10/[$LATEST]abcdef",
		"logEvents": []map[string]any{
			{"timestamp": int64(1775800000000), "message": "first line"},
			{"timestamp": int64(1775800001000), "message": "second line"},
		},
	})

	groups, err := svc.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "/aws/lambda/test-func" {
		t.Fatalf("unexpected groups: %+v", groups)
	}

	streams, err := svc.ListStreams("/aws/lambda/test-func")
	if err != nil {
		t.Fatalf("list streams: %v", err)
	}
	if len(streams) != 1 || streams[0].StreamName != "2026/04/10/[$LATEST]abcdef" {
		t.Fatalf("unexpected streams: %+v", streams)
	}

	events, err := svc.ListEvents("/aws/lambda/test-func", "2026/04/10/[$LATEST]abcdef", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %+v", events)
	}
	if events[0].Message != "second line" || events[1].Message != "first line" {
		t.Fatalf("unexpected events ordering: %+v", events)
	}
}

func TestCreateLogStreamRequiresExistingGroup(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)
	body := bytes.NewBufferString(`{"logGroupName":"/aws/lambda/missing","logStreamName":"stream-1"}`)
	req := httptest.NewRequest("POST", "http://localhost:4566/", body)
	rec := httptest.NewRecorder()

	err = svc.Handle(rec, req, "CreateLogStream")
	if err == nil {
		t.Fatal("expected missing group error")
	}
	apiErr, ok := err.(*apierror.Error)
	if !ok {
		t.Fatalf("unexpected error type: %T", err)
	}
	if apiErr.Code != "ResourceNotFoundException" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
}
