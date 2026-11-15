package lambda

import (
	"context"
	"sync"
	"testing"
	"time"
)

type scriptedInvoker struct{ result InvokeResult }

func (s scriptedInvoker) Invoke(context.Context, FunctionSpec, []byte) (InvokeResult, error) {
	return s.result, nil
}
func (scriptedInvoker) CleanupFunction(context.Context, string) error { return nil }
func (scriptedInvoker) Close() error                                  { return nil }

type memorySink struct {
	mu            sync.Mutex
	group, stream string
	lines         []string
	calls         int
}

func (m *memorySink) Append(group, stream string, lines []string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.group, m.stream, m.lines = group, stream, lines
	return nil
}

func TestInvocationOutputGoesToTheFunctionsLogGroup(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "process-order")
	svc.invoker = scriptedInvoker{result: InvokeResult{
		Payload: []byte(`{}`),
		Logs:    "START RequestId: 1\nfailed m-1: Float types are not supported\nEND RequestId: 1\n",
	}}
	sink := &memorySink{}
	svc.SetLogSink(sink)
	svc.now = func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) }

	if _, err := svc.InvokeByName(context.Background(), "process-order", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if sink.group != "/aws/lambda/process-order" || sink.stream != "2026/11/15/[$LATEST]stratus" {
		t.Fatalf("shipped to %s %s", sink.group, sink.stream)
	}
	if len(sink.lines) != 3 || sink.lines[1] != "failed m-1: Float types are not supported" {
		t.Fatalf("lines: %q", sink.lines)
	}
}

func TestEmptyOutputShipsNothing(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "quiet")
	svc.invoker = scriptedInvoker{result: InvokeResult{Payload: []byte(`{}`)}}
	sink := &memorySink{}
	svc.SetLogSink(sink)
	if _, err := svc.InvokeByName(context.Background(), "quiet", nil); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 0 {
		t.Fatalf("shipped %d times", sink.calls)
	}
}
