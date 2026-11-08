package stepfunctions

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/chaos/chaostest"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

type okInvoker struct{ calls int }

func (i *okInvoker) Invoke(context.Context, lambdasvc.FunctionSpec, []byte) (lambdasvc.InvokeResult, error) {
	i.calls++
	return lambdasvc.InvokeResult{Payload: []byte(`{"charged":true}`)}, nil
}
func (*okInvoker) CleanupFunction(context.Context, string) error { return nil }
func (*okInvoker) Close() error                                  { return nil }

func TestTaskRetriesAfterAnInjectedInvokeError(t *testing.T) {
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	invoker := &okInvoker{}
	lambda := lambdasvc.NewService(lambdasvc.Options{Metadata: meta, Blobs: fsblob.New(t.TempDir()), Invoker: invoker})
	if _, err := lambda.Provision(lambdasvc.ProvisionInput{
		CodeZip: emptyZip(t), FunctionName: "charge", Handler: "index.handler", Role: "arn:aws:iam::000000000000:role/demo", Runtime: "nodejs20.x", Timeout: 3,
	}); err != nil {
		t.Fatal(err)
	}

	gate := &chaostest.Gate{Outcome: func(n int, _ chaos.Delivery) error {
		if n == 1 {
			return &chaos.FaultError{Spec: chaos.ErrorSpec{StatusCode: 500, Code: "ServiceException"}}
		}
		return nil
	}}
	svc := NewService(meta, lambda)
	svc.SetFaults(gate)

	const fnARN = "arn:aws:lambda:us-east-1:000000000000:function:charge"
	const machineARN = "arn:aws:states:us-east-1:000000000000:stateMachine:checkout"
	definition := stateMachineDefinition{StartAt: "Charge", States: map[string]definitionNode{
		"Charge": {Type: "Task", Resource: fnARN, Retry: []retrier{{ErrorEquals: []string{"States.ALL"}, MaxAttempts: 2}}, End: true},
	}}

	out, err := svc.executeDefinition(machineARN, definition, map[string]any{})
	if err != nil {
		t.Fatalf("the retry should absorb the injected error: %v", err)
	}
	if out.(map[string]any)["charged"] != true || invoker.calls != 1 {
		t.Fatalf("out=%v calls=%d", out, invoker.calls)
	}
	got := gate.Deliveries()
	if len(got) != 2 {
		t.Fatalf("want 2 attempts through the gate, got %d", len(got))
	}
	want := chaos.Delivery{Source: "stepfunctions", SourceARN: machineARN, Service: "lambda", Operation: "Invoke", TargetARN: fnARN, Function: "charge"}
	if got[0] != want {
		t.Fatalf("delivery = %+v, want %+v", got[0], want)
	}
}

func emptyZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := zip.NewWriter(&buf).Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
