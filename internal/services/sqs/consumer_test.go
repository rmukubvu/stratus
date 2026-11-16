package sqs

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

const ordersARN = "arn:aws:sqs:us-east-1:000000000000:orders"

type recordingListener struct {
	mu     sync.Mutex
	queues []string
}

func (l *recordingListener) QueueMessagesAvailable(queueARN string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queues = append(l.queues, queueARN)
}

// clock lets a test move the service's time past visibility timeouts.
type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func withClock(h *harness) *clock {
	c := &clock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	h.svc.now = func() time.Time { return c.now }
	return c
}

func (h *harness) send(bodies ...string) {
	h.t.Helper()
	for _, body := range bodies {
		if _, err := h.query("SendMessage", url.Values{"MessageBody": {body}}); err != nil {
			h.t.Fatalf("send: %v", err)
		}
	}
}

func TestReceiveFromARNStartsTheVisibilityTimeout(t *testing.T) {
	h := newHarness(t, "VisibilityTimeout", "5")
	c := withClock(h)
	h.send("a")
	c.advance(time.Millisecond)
	h.send("b")

	batch, err := h.svc.ReceiveFromARN(ordersARN, 10)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if len(batch) != 2 || batch[0].Body != "a" || batch[0].ReceiptHandle == "" || batch[0].MD5OfBody == "" {
		t.Fatalf("unexpected batch: %+v", batch)
	}
	attrs := batch[0].Attributes
	if attrs["ApproximateReceiveCount"] != "1" || attrs["SentTimestamp"] == "" || attrs["ApproximateFirstReceiveTimestamp"] == "" || attrs["SenderId"] == "" {
		t.Fatalf("unexpected attributes: %v", attrs)
	}
	if again, _ := h.svc.ReceiveFromARN(ordersARN, 10); len(again) != 0 {
		t.Fatalf("in-flight messages were received again: %+v", again)
	}

	c.advance(6 * time.Second)
	retried, _ := h.svc.ReceiveFromARN(ordersARN, 1)
	if len(retried) != 1 || retried[0].Attributes["ApproximateReceiveCount"] != "2" {
		t.Fatalf("expected one message back after the timeout, got %+v", retried)
	}
}

func TestDeleteFromARNSkipsStaleHandles(t *testing.T) {
	h := newHarness(t, "VisibilityTimeout", "5")
	c := withClock(h)
	h.send("a")
	c.advance(time.Millisecond)
	h.send("b")
	batch, _ := h.svc.ReceiveFromARN(ordersARN, 10)

	c.advance(6 * time.Second)
	fresh, _ := h.svc.ReceiveFromARN(ordersARN, 1) // "a" again, with a new handle

	if err := h.svc.DeleteFromARN(ordersARN, []string{batch[0].ReceiptHandle, batch[1].ReceiptHandle}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// "b" was deleted; "a" kept, because its old handle went stale.
	if err := h.svc.DeleteFromARN(ordersARN, []string{fresh[0].ReceiptHandle}); err != nil {
		t.Fatalf("delete fresh: %v", err)
	}
	c.advance(time.Minute)
	if left, _ := h.svc.ReceiveFromARN(ordersARN, 10); len(left) != 0 {
		t.Fatalf("messages left: %+v", left)
	}
}

func TestReceiveFromARNRedrivesToTheDeadLetterQueue(t *testing.T) {
	h := newHarness(t, "VisibilityTimeout", "1")
	c := withClock(h)
	if _, err := h.query("CreateQueue", url.Values{"QueueName": {"orders-dlq"}}); err != nil {
		t.Fatalf("create dlq: %v", err)
	}
	if _, err := h.query("SetQueueAttributes", url.Values{
		"Attribute.1.Name":  {"RedrivePolicy"},
		"Attribute.1.Value": {`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:orders-dlq","maxReceiveCount":"2"}`},
	}); err != nil {
		t.Fatalf("set redrive: %v", err)
	}
	h.send("poison")

	for i := 0; i < 2; i++ {
		if got, _ := h.svc.ReceiveFromARN(ordersARN, 1); len(got) != 1 {
			t.Fatalf("receive %d: got %d messages", i+1, len(got))
		}
		c.advance(2 * time.Second)
	}
	if got, _ := h.svc.ReceiveFromARN(ordersARN, 1); len(got) != 0 {
		t.Fatalf("the message should have been redriven, got %+v", got)
	}
	dlq, err := h.svc.ReceiveFromARN("arn:aws:sqs:us-east-1:000000000000:orders-dlq", 10)
	if err != nil || len(dlq) != 1 || dlq[0].Body != "poison" {
		t.Fatalf("dead-letter queue = %+v, %v", dlq, err)
	}
}

func TestSendingWakesTheListener(t *testing.T) {
	h := newHarness(t)
	listener := &recordingListener{}
	h.svc.SetMessageListener(listener)

	h.send("a")
	if err := h.svc.SendMessageToARN(ordersARN, "b"); err != nil {
		t.Fatalf("send to arn: %v", err)
	}
	if len(listener.queues) != 2 || listener.queues[0] != ordersARN || listener.queues[1] != ordersARN {
		t.Fatalf("listener heard %v", listener.queues)
	}
}

func TestConsumerReportsMissingQueues(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.ReceiveFromARN("arn:aws:sqs:us-east-1:000000000000:missing", 1); err == nil {
		t.Fatalf("receiving from a missing queue should fail")
	}
	if err := h.svc.DeleteFromARN("arn:aws:sqs:us-east-1:000000000000:missing", []string{"h"}); err == nil {
		t.Fatalf("deleting from a missing queue should fail")
	}
}
