package operator

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/services/logs"
	"github.com/stratus/internal/store/bbolt"
)

func TestBootstrapIncludesServicesAndExamples(t *testing.T) {
	store := NewStore("http://127.0.0.1:4566", "/tmp/stratus-data", "debug", "pretty")
	svc := NewService(store, nil)

	payload := svc.Bootstrap()
	if payload.Overview.Endpoint != "http://127.0.0.1:4566" {
		t.Fatalf("endpoint = %q", payload.Overview.Endpoint)
	}
	if len(payload.Services) == 0 {
		t.Fatal("expected supported services")
	}
	if len(payload.Examples) == 0 {
		t.Fatal("expected quick start examples")
	}
	if payload.PortalURL != "http://127.0.0.1:4566/_stratus/" {
		t.Fatalf("portal url = %q", payload.PortalURL)
	}
}

func TestPortalHTMLRendersBootstrapContent(t *testing.T) {
	store := NewStore("http://127.0.0.1:4566", "/tmp/stratus-data", "debug", "pretty")
	svc := NewService(store, nil)

	page, err := svc.PortalHTML()
	if err != nil {
		t.Fatalf("portal html: %v", err)
	}
	body := string(page)
	for _, want := range []string{
		"Local Operator Portal",
		"Supported Services",
		"Connect to Stratus",
		"/_stratus/operator/bootstrap",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("portal html missing %q", want)
		}
	}
}

func TestHandleBootstrapEndpoint(t *testing.T) {
	store := NewStore("http://127.0.0.1:4566", "/tmp/stratus-data", "info", "json")
	svc := NewService(store, nil)
	req := httptest.NewRequest("GET", "http://localhost:4566/_stratus/operator/bootstrap", nil)

	status, payload := svc.Handle(httptest.NewRecorder(), req)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	data, ok := payload.(bootstrapPayload)
	if !ok {
		t.Fatalf("payload type = %T", payload)
	}
	if len(data.Services) == 0 || len(data.Examples) == 0 {
		t.Fatalf("unexpected bootstrap payload: %+v", data)
	}
}

func TestHandleLogEndpoints(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	logsService := logs.NewService(metadata)
	mustWrite := func(operation string, payload any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		if err := logsService.Handle(rec, req, operation); err != nil {
			t.Fatalf("%s failed: %v", operation, err)
		}
	}

	mustWrite("CreateLogGroup", map[string]any{"logGroupName": "/aws/lambda/operator"})
	mustWrite("CreateLogStream", map[string]any{
		"logGroupName":  "/aws/lambda/operator",
		"logStreamName": "stream-1",
	})
	mustWrite("PutLogEvents", map[string]any{
		"logGroupName":  "/aws/lambda/operator",
		"logStreamName": "stream-1",
		"logEvents":     []map[string]any{{"timestamp": int64(1775800000000), "message": "hello"}},
	})

	store := NewStore("http://127.0.0.1:4566", t.TempDir(), "debug", "pretty")
	svc := NewService(store, logsService)

	req := httptest.NewRequest("GET", "http://localhost:4566/_stratus/operator/logs/groups", nil)
	status, payload := svc.Handle(httptest.NewRecorder(), req)
	if status != 200 {
		t.Fatalf("groups status = %d", status)
	}
	groups := payload.(map[string]any)["items"].([]logs.GroupSummary)
	if len(groups) != 1 || groups[0].Name != "/aws/lambda/operator" {
		t.Fatalf("unexpected groups: %+v", groups)
	}

	req = httptest.NewRequest("GET", "http://localhost:4566/_stratus/operator/logs/streams?group=%2Faws%2Flambda%2Foperator", nil)
	status, payload = svc.Handle(httptest.NewRecorder(), req)
	if status != 200 {
		t.Fatalf("streams status = %d", status)
	}
	streams := payload.(map[string]any)["items"].([]logs.StreamSummary)
	if len(streams) != 1 || streams[0].StreamName != "stream-1" {
		t.Fatalf("unexpected streams: %+v", streams)
	}

	req = httptest.NewRequest("GET", "http://localhost:4566/_stratus/operator/logs/events?group=%2Faws%2Flambda%2Foperator&stream=stream-1&limit=20", nil)
	status, payload = svc.Handle(httptest.NewRecorder(), req)
	if status != 200 {
		t.Fatalf("events status = %d", status)
	}
	events := payload.(map[string]any)["items"].([]logs.EventSummary)
	if len(events) != 1 || events[0].Message != "hello" {
		t.Fatalf("unexpected events: %+v", events)
	}
}
