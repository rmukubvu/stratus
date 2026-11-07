package sns

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/chaos/chaostest"
	"github.com/stratus/internal/store/bbolt"
)

type recordingQueue struct{ sent []string }

func (q *recordingQueue) SendMessageToARN(_ string, body string) error {
	q.sent = append(q.sent, body)
	return nil
}

func TestFanOutToQueuesGoesThroughTheChaosGate(t *testing.T) {
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	svc := NewService(meta)
	queue := &recordingQueue{}
	gate := &chaostest.Gate{Outcome: func(n int, _ chaos.Delivery) error {
		if n == 1 {
			return &chaos.FaultError{Spec: chaos.ErrorSpec{Code: "InternalFailure"}}
		}
		return nil
	}}
	svc.SetQueuePublisher(queue)
	svc.SetFaults(gate)

	topic, err := svc.CreateTopic(CreateTopicInput{Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	const queueARN = "arn:aws:sqs:us-east-1:000000000000:audit"
	form := url.Values{"Action": {"Subscribe"}, "TopicArn": {topic}, "Protocol": {"sqs"}, "Endpoint": {queueARN}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := svc.Handle(httptest.NewRecorder(), req, "Subscribe", "req-1"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// The gate fails the first delivery and lets the second through.
	if err := svc.PublishToTopic(topic, "first"); err == nil {
		t.Fatal("SNS should return what the gate returns (the real injector absorbs injected errors on async paths)")
	}
	if err := svc.PublishToTopic(topic, "second"); err != nil {
		t.Fatal(err)
	}

	if len(queue.sent) != 1 || !strings.Contains(queue.sent[0], "second") {
		t.Fatalf("only the second message should reach the queue: %v", queue.sent)
	}
	got := gate.Deliveries()[0]
	want := chaos.Delivery{Source: "sns", SourceARN: topic, Service: "sqs", Operation: "SendMessage", TargetARN: queueARN, Async: true}
	if got != want {
		t.Fatalf("delivery = %+v, want %+v", got, want)
	}
}
