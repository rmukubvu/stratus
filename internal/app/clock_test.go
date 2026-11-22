package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stratus/internal/config"
)

// emulator is a real Stratus on a loopback port, for tests that drive it
// over HTTP the way an SDK and the control API do.
type emulator struct {
	t    *testing.T
	base string
}

func startEmulator(t *testing.T) *emulator {
	t.Helper()
	cfg := config.Config{Port: 1, DataDir: t.TempDir(), LogLevel: "error", LogFormat: "json"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	application, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return &emulator{t: t, base: "http://" + ln.Addr().String()}
}

func (e *emulator) call(method, path, service string, headers map[string]string, body any) map[string]any {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.base+path, bytes.NewReader(raw))
	if service != "" {
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261122/us-east-1/"+service+"/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode >= 300 {
		e.t.Fatalf("%s %s (%s): %d %s", method, path, service, resp.StatusCode, data)
	}
	return out
}

func (e *emulator) sqs(op string, body any) map[string]any {
	e.t.Helper()
	return e.call("POST", "/", "sqs", map[string]string{
		"X-Amz-Target": "AmazonSQS." + op, "Content-Type": "application/x-amz-json-1.0",
	}, body)
}

func (e *emulator) clock(path string, body any) map[string]any {
	e.t.Helper()
	method := "POST"
	if path == "" {
		method = "GET"
	}
	return e.call(method, "/_stratus/control/v1/clock"+path, "", nil, body)
}

func (e *emulator) receive(queueURL string) []any {
	e.t.Helper()
	out := e.sqs("ReceiveMessage", map[string]any{"QueueUrl": queueURL, "MaxNumberOfMessages": 10, "VisibilityTimeout": 300})
	messages, _ := out["Messages"].([]any)
	return messages
}

func TestAdvancingTheClockCrossesAnSQSVisibilityTimeoutWithoutWaiting(t *testing.T) {
	e := startEmulator(t)
	queueURL, _ := e.sqs("CreateQueue", map[string]any{"QueueName": "orders"})["QueueUrl"].(string)
	e.sqs("SendMessage", map[string]any{"QueueUrl": queueURL, "MessageBody": "order-1"})

	if got := e.receive(queueURL); len(got) != 1 {
		t.Fatalf("first receive got %d messages, want 1", len(got))
	}
	if got := e.receive(queueURL); len(got) != 0 {
		t.Fatalf("message visible again inside its 5m visibility timeout")
	}

	e.clock("/advance", map[string]any{"by": "5m1s"})
	if got := e.receive(queueURL); len(got) != 1 {
		t.Fatalf("after advancing past the visibility timeout got %d messages, want the message back", len(got))
	}
}

func TestAdvancingTheClockFiresDueSchedulesBeforeReturning(t *testing.T) {
	e := startEmulator(t)
	queueURL, _ := e.sqs("CreateQueue", map[string]any{"QueueName": "reports"})["QueueUrl"].(string)
	e.call("POST", "/schedules/hourly-report", "scheduler", map[string]string{"Content-Type": "application/json"}, map[string]any{
		"ScheduleExpression": "rate(1 hour)",
		"FlexibleTimeWindow": map[string]any{"Mode": "OFF"},
		"Target": map[string]any{
			"Arn":     "arn:aws:sqs:us-east-1:000000000000:reports",
			"RoleArn": "arn:aws:iam::000000000000:role/scheduler",
			"Input":   `{"report":"hourly"}`,
		},
	})

	moved := e.clock("/advance", map[string]any{"by": "61m"})
	effects, _ := moved["effects"].([]any)
	if len(effects) != 1 || effects[0].(map[string]any)["source"] != "scheduler" {
		t.Fatalf("effects = %v, want the scheduler firing", moved["effects"])
	}
	if got := e.receive(queueURL); len(got) != 1 {
		t.Fatalf("got %d scheduled messages, want 1", len(got))
	}
}

func TestAFrozenClockStampsEveryRecordWithTheSameTime(t *testing.T) {
	e := startEmulator(t)
	frozen := e.clock("/freeze", nil)
	queueURL, _ := e.sqs("CreateQueue", map[string]any{"QueueName": "audit"})["QueueUrl"].(string)
	time.Sleep(1100 * time.Millisecond)
	attrs := e.sqs("GetQueueAttributes", map[string]any{"QueueUrl": queueURL, "AttributeNames": []string{"CreatedTimestamp"}})

	at, _ := time.Parse(time.RFC3339Nano, frozen["now"].(string))
	created, _ := attrs["Attributes"].(map[string]any)["CreatedTimestamp"].(string)
	if want := strconv.FormatInt(at.Unix(), 10); created != want {
		t.Fatalf("CreatedTimestamp = %s, want the frozen clock's %s", created, want)
	}
	if now := e.clock("", nil)["now"]; now != frozen["now"] {
		t.Fatalf("frozen clock moved from %v to %v", frozen["now"], now)
	}
}
