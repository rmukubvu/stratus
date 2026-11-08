package events

import (
	"context"
	"errors"
	"testing"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/chaos/chaostest"
)

func TestTargetDeliveriesAreDescribedToTheChaosGate(t *testing.T) {
	const rule = "arn:aws:events:us-east-1:000000000000:rule/orders"
	stop := errors.New("stop before reaching the target")
	cases := []struct {
		arn, service, operation, function string
	}{
		{"arn:aws:lambda:us-east-1:000000000000:function:ship", "lambda", "Invoke", "ship"},
		{"arn:aws:sqs:us-east-1:000000000000:audit", "sqs", "SendMessage", ""},
		{"arn:aws:sns:us-east-1:000000000000:alerts", "sns", "Publish", ""},
		{"arn:aws:states:us-east-1:000000000000:stateMachine:refund", "stepfunctions", "StartExecution", ""},
	}
	for _, tc := range cases {
		gate := &chaostest.Gate{Outcome: func(int, chaos.Delivery) error { return stop }}
		svc := &Service{faults: gate}
		err := svc.deliverTarget(context.Background(), rule, targetRecord{Arn: tc.arn}, []byte(`{}`))
		if !errors.Is(err, stop) {
			t.Fatalf("%s: gate outcome not returned: %v", tc.arn, err)
		}
		want := chaos.Delivery{Source: "events", SourceARN: rule, Service: tc.service, Operation: tc.operation,
			TargetARN: tc.arn, Function: tc.function, Async: true}
		if got := gate.Deliveries()[0]; got != want {
			t.Errorf("delivery = %+v, want %+v", got, want)
		}
	}
}
