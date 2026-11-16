package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/chaos"
)

const testQueueARN = "arn:aws:sqs:us-east-1:000000000000:orders"

// fakeQueue is an in-memory queue with the visibility semantics the poller
// relies on: received messages are hidden until expire is called.
type fakeQueue struct {
	mu       sync.Mutex
	messages []*fakeMessage
	next     int
}

type fakeMessage struct {
	id       string
	body     string
	handle   string
	inflight bool
	receives int
}

func newFakeQueue(bodies ...string) *fakeQueue {
	q := &fakeQueue{}
	for _, body := range bodies {
		q.send(body)
	}
	return q
}

func (q *fakeQueue) send(body string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next++
	q.messages = append(q.messages, &fakeMessage{id: fmt.Sprintf("m%d", q.next), body: body})
}

func (q *fakeQueue) ReceiveFromARN(_ string, max int) ([]QueueMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []QueueMessage
	for _, m := range q.messages {
		if len(out) == max {
			break
		}
		if m.inflight {
			continue
		}
		q.next++
		m.inflight, m.receives, m.handle = true, m.receives+1, fmt.Sprintf("h%d", q.next)
		out = append(out, QueueMessage{
			MessageID:     m.id,
			ReceiptHandle: m.handle,
			Body:          m.body,
			Attributes:    map[string]string{"ApproximateReceiveCount": fmt.Sprint(m.receives)},
		})
	}
	return out, nil
}

func (q *fakeQueue) DeleteFromARN(_ string, handles []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	drop := map[string]bool{}
	for _, h := range handles {
		drop[h] = true
	}
	kept := q.messages[:0]
	for _, m := range q.messages {
		if !(m.inflight && drop[m.handle]) {
			kept = append(kept, m)
		}
	}
	q.messages = kept
	return nil
}

// expire ends every visibility timeout, as time passing would.
func (q *fakeQueue) expire() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.messages {
		m.inflight = false
	}
}

func (q *fakeQueue) remaining() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.messages))
	for _, m := range q.messages {
		out = append(out, m.body)
	}
	sort.Strings(out)
	return out
}

// batchInvoker answers each invocation with respond, recording events.
type batchInvoker struct {
	mu      sync.Mutex
	events  []sqsTestEvent
	respond func(sqsTestEvent) (InvokeResult, error)
}

type sqsTestEvent struct {
	Records []struct {
		MessageID      string            `json:"messageId"`
		ReceiptHandle  string            `json:"receiptHandle"`
		Body           string            `json:"body"`
		Attributes     map[string]string `json:"attributes"`
		EventSource    string            `json:"eventSource"`
		EventSourceARN string            `json:"eventSourceARN"`
		AWSRegion      string            `json:"awsRegion"`
	} `json:"Records"`
}

func (s *batchInvoker) Invoke(_ context.Context, _ FunctionSpec, payload []byte) (InvokeResult, error) {
	var event sqsTestEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return InvokeResult{}, err
	}
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	if s.respond == nil {
		return InvokeResult{Payload: []byte(`null`)}, nil
	}
	return s.respond(event)
}
func (s *batchInvoker) CleanupFunction(context.Context, string) error { return nil }
func (s *batchInvoker) Close() error                                  { return nil }

func (s *batchInvoker) invocations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// gateFunc adapts a function to chaos.Gate.
type gateFunc func(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error

func (g gateFunc) Deliver(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
	return g(ctx, d, deliver)
}

func pollerService(t *testing.T, queue *fakeQueue, inv *batchInvoker) *Service {
	t.Helper()
	svc := newTestService(t)
	svc.invoker = inv
	svc.SetQueueConsumer(queue)
	provisionTestFunction(t, svc, "worker")
	return svc
}

func createSQSMapping(t *testing.T, svc *Service, body string) eventSourceMappingRecord {
	t.Helper()
	rec, err := call(t, svc, http.MethodPost, "/2015-03-31/event-source-mappings", "CreateEventSourceMapping", body)
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	var out struct{ UUID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mapping: %v", err)
	}
	mapping, err := svc.loadEventSourceMapping(out.UUID)
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	return mapping
}

func defaultMappingBody(extra string) string {
	return `{"FunctionName":"worker","EventSourceArn":"` + testQueueARN + `"` + extra + `}`
}

func TestPollSQSBatchDeletesMessagesAfterSuccess(t *testing.T) {
	queue := newFakeQueue("a", "b", "c")
	inv := &batchInvoker{}
	svc := pollerService(t, queue, inv)
	mapping := createSQSMapping(t, svc, defaultMappingBody(`,"BatchSize":2`))

	if got := svc.pollSQSBatch(context.Background(), mapping); got != 2 {
		t.Fatalf("received %d messages, want a batch of 2", got)
	}
	if got := queue.remaining(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("remaining = %v, want only the message outside the batch", got)
	}
	event := inv.events[0]
	if len(event.Records) != 2 {
		t.Fatalf("event has %d records, want 2", len(event.Records))
	}
	first := event.Records[0]
	if first.EventSource != "aws:sqs" || first.EventSourceARN != testQueueARN || first.Body != "a" || first.ReceiptHandle == "" || first.AWSRegion == "" {
		t.Fatalf("unexpected record: %+v", first)
	}
	if first.Attributes["ApproximateReceiveCount"] != "1" {
		t.Fatalf("attributes = %v", first.Attributes)
	}
}

func TestPollSQSBatchKeepsTheBatchWhenTheFunctionFails(t *testing.T) {
	queue := newFakeQueue("a", "b")
	inv := &batchInvoker{respond: func(sqsTestEvent) (InvokeResult, error) {
		return InvokeResult{FunctionError: "Unhandled", Payload: []byte(`{"errorType":"ValueError","errorMessage":"boom"}`)}, nil
	}}
	svc := pollerService(t, queue, inv)
	mapping := createSQSMapping(t, svc, defaultMappingBody(""))

	svc.pollSQSBatch(context.Background(), mapping)
	if got := queue.remaining(); len(got) != 2 {
		t.Fatalf("remaining = %v, want the whole batch kept for retry", got)
	}
	// Nothing is redelivered while the visibility timeout runs...
	if got := svc.pollSQSBatch(context.Background(), mapping); got != 0 {
		t.Fatalf("received %d in-flight messages", got)
	}
	// ...and everything is retried once it ends, now succeeding.
	queue.expire()
	inv.respond = nil
	svc.pollSQSBatch(context.Background(), mapping)
	if got := queue.remaining(); len(got) != 0 {
		t.Fatalf("remaining = %v after a successful retry", got)
	}
	if got := inv.events[1].Records[0].Attributes["ApproximateReceiveCount"]; got != "2" {
		t.Fatalf("retry receive count = %s, want 2", got)
	}
}

func TestPollSQSBatchHonoursReportBatchItemFailures(t *testing.T) {
	queue := newFakeQueue("a", "b", "c")
	inv := &batchInvoker{respond: func(event sqsTestEvent) (InvokeResult, error) {
		failed := event.Records[1].MessageID
		return InvokeResult{Payload: []byte(`{"batchItemFailures":[{"itemIdentifier":"` + failed + `"}]}`)}, nil
	}}
	svc := pollerService(t, queue, inv)
	mapping := createSQSMapping(t, svc, defaultMappingBody(`,"FunctionResponseTypes":["ReportBatchItemFailures"]`))

	svc.pollSQSBatch(context.Background(), mapping)
	if got := queue.remaining(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("remaining = %v, want only the reported failure", got)
	}
}

func TestPollSQSBatchIgnoresPartialResponsesUnlessEnabled(t *testing.T) {
	queue := newFakeQueue("a", "b")
	inv := &batchInvoker{respond: func(event sqsTestEvent) (InvokeResult, error) {
		return InvokeResult{Payload: []byte(`{"batchItemFailures":[{"itemIdentifier":"` + event.Records[0].MessageID + `"}]}`)}, nil
	}}
	svc := pollerService(t, queue, inv)
	mapping := createSQSMapping(t, svc, defaultMappingBody(""))

	svc.pollSQSBatch(context.Background(), mapping)
	if got := queue.remaining(); len(got) != 0 {
		t.Fatalf("remaining = %v, want every message deleted", got)
	}
}

func TestPollSQSBatchKeepsTheBatchWhenChaosFailsOrDropsTheDelivery(t *testing.T) {
	cases := map[string]gateFunc{
		"injected error": func(context.Context, chaos.Delivery, func(context.Context) error) error {
			return &chaos.FaultError{}
		},
		"dropped": func(context.Context, chaos.Delivery, func(context.Context) error) error {
			return nil
		},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			queue := newFakeQueue("a")
			inv := &batchInvoker{}
			svc := pollerService(t, queue, inv)
			svc.SetFaults(g)
			mapping := createSQSMapping(t, svc, defaultMappingBody(""))

			svc.pollSQSBatch(context.Background(), mapping)
			if got := queue.remaining(); len(got) != 1 {
				t.Fatalf("remaining = %v, want the message kept", got)
			}
			if inv.invocations() != 0 {
				t.Fatalf("function was invoked %d times", inv.invocations())
			}
		})
	}
}

func TestPollSQSBatchDeliversSynchronouslyThroughTheGate(t *testing.T) {
	queue := newFakeQueue("a")
	svc := pollerService(t, queue, &batchInvoker{})
	var seen chaos.Delivery
	svc.SetFaults(gateFunc(func(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
		seen = d
		return deliver(ctx)
	}))
	mapping := createSQSMapping(t, svc, defaultMappingBody(""))

	svc.pollSQSBatch(context.Background(), mapping)
	if seen.Async || seen.Source != "sqs" || seen.SourceARN != testQueueARN || seen.Function != "worker" || seen.Operation != "Invoke" {
		t.Fatalf("unexpected delivery: %+v", seen)
	}
}

func TestPollSQSBatchReturnsTheBatchWhenThrottled(t *testing.T) {
	queue := newFakeQueue("a")
	inv := &batchInvoker{}
	svc := pollerService(t, queue, inv)
	svc.SetConcurrency(&gate{throttle: 1})
	mapping := createSQSMapping(t, svc, defaultMappingBody(""))

	svc.pollSQSBatch(context.Background(), mapping)
	if got := queue.remaining(); len(got) != 1 || inv.invocations() != 0 {
		t.Fatalf("remaining = %v, invocations = %d; want the throttled batch kept", got, inv.invocations())
	}
}

func TestPollingDrainsTheBacklogAndStopsOnClose(t *testing.T) {
	bodies := make([]string, 25)
	for i := range bodies {
		bodies[i] = fmt.Sprintf("backlog-%02d", i)
	}
	queue := newFakeQueue(bodies...)
	inv := &batchInvoker{}
	svc := pollerService(t, queue, inv)
	createSQSMapping(t, svc, defaultMappingBody(`,"BatchSize":3`))

	svc.StartPolling()
	waitFor(t, func() bool { return len(queue.remaining()) == 0 })

	queue.send("late")
	svc.QueueMessagesAvailable(testQueueARN)
	waitFor(t, func() bool { return len(queue.remaining()) == 0 })

	if err := svc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	queue.send("after close")
	time.Sleep(3 * sqsPollInterval)
	if got := queue.remaining(); len(got) != 1 {
		t.Fatalf("remaining = %v, want polling stopped", got)
	}
}

func TestPollingSkipsDisabledMappings(t *testing.T) {
	queue := newFakeQueue("a")
	inv := &batchInvoker{}
	svc := pollerService(t, queue, inv)
	createSQSMapping(t, svc, defaultMappingBody(`,"Enabled":false`))

	svc.StartPolling()
	t.Cleanup(func() { _ = svc.Close() })
	time.Sleep(3 * sqsPollInterval)
	if inv.invocations() != 0 || len(queue.remaining()) != 1 {
		t.Fatalf("a disabled mapping polled its queue")
	}
}

func TestPollerScalesUpToMaximumConcurrency(t *testing.T) {
	bodies := make([]string, 40)
	for i := range bodies {
		bodies[i] = fmt.Sprint(i)
	}
	queue := newFakeQueue(bodies...)
	var (
		mu            sync.Mutex
		running, peak int
	)
	inv := &batchInvoker{respond: func(sqsTestEvent) (InvokeResult, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return InvokeResult{Payload: []byte(`null`)}, nil
	}}
	svc := pollerService(t, queue, inv)
	createSQSMapping(t, svc, defaultMappingBody(`,"BatchSize":1,"ScalingConfig":{"MaximumConcurrency":2}`))

	svc.StartPolling()
	t.Cleanup(func() { _ = svc.Close() })
	waitFor(t, func() bool { return len(queue.remaining()) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
}

func TestReceiveSQSBatchWaitsOutTheBatchingWindow(t *testing.T) {
	queue := newFakeQueue("a")
	svc := pollerService(t, queue, &batchInvoker{})
	mapping := createSQSMapping(t, svc, defaultMappingBody(`,"BatchSize":3,"MaximumBatchingWindowInSeconds":1`))

	go func() {
		time.Sleep(150 * time.Millisecond)
		queue.send("b")
		queue.send("c")
	}()
	start := time.Now()
	batch := svc.receiveSQSBatch(context.Background(), mapping)
	if len(batch) != 3 {
		t.Fatalf("batch has %d messages, want the window to gather 3", len(batch))
	}
	if time.Since(start) >= time.Second {
		t.Fatalf("a full batch should not wait for the window to close")
	}
}

func TestDeliverSQSBatchReportsInvalidPartialResponses(t *testing.T) {
	queue := newFakeQueue("a")
	inv := &batchInvoker{respond: func(sqsTestEvent) (InvokeResult, error) {
		return InvokeResult{Payload: []byte(`{"batchItemFailures":[{"itemIdentifier":"unknown"}]}`)}, nil
	}}
	svc := pollerService(t, queue, inv)
	mapping := createSQSMapping(t, svc, defaultMappingBody(`,"FunctionResponseTypes":["ReportBatchItemFailures"]`))
	batch, _ := queue.ReceiveFromARN(testQueueARN, 1)

	if _, err := svc.deliverSQSBatch(context.Background(), mapping, batch); err == nil {
		t.Fatalf("an invalid partial batch response should fail the batch")
	}
	var fnErr *FunctionError
	inv.respond = func(sqsTestEvent) (InvokeResult, error) {
		return InvokeResult{FunctionError: "Unhandled", Payload: []byte(`{"errorType":"KeyError","errorMessage":"id"}`)}, nil
	}
	if _, err := svc.deliverSQSBatch(context.Background(), mapping, batch); !errors.As(err, &fnErr) || fnErr.Type != "KeyError" {
		t.Fatalf("function errors should surface as *FunctionError, got %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
