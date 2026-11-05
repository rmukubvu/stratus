package bench

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/ci"
)

func TestPercentilesUseNearestRank(t *testing.T) {
	r := Result{Samples: []time.Duration{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}}
	if got := r.Median(); got != 5 {
		t.Fatalf("median = %v, want 5", got)
	}
	if got := r.P90(); got != 9 {
		t.Fatalf("p90 = %v, want 9", got)
	}
	if (Result{}).Median() != 0 {
		t.Fatal("empty result should have zero median")
	}
}

func TestWriteMarkdownComparesToTheFirstStrategy(t *testing.T) {
	results := []Result{
		{Strategy: "slow", Description: "a", Samples: []time.Duration{100 * time.Millisecond}},
		{Strategy: "fast", Description: "b", Samples: []time.Duration{10 * time.Millisecond}},
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, DefaultFixture(), 1, 100, results); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"| slow | a | 100.0 ms | 100.0 ms | 1.0x | 10.0 s |",
		"| fast | b | 10.0 ms | 10.0 ms | 10.0x | 1.0 s |",
		"16 KiB each",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// tinyFixture keeps the end-to-end runs fast.
func tinyFixture() Fixture {
	return Fixture{Buckets: 2, ObjectsPerBucket: 3, ObjectBytes: 64, Tables: 1, ItemsPerTable: 30, Queues: 1, MessagesPerQueue: 2}
}

func realHost(t *testing.T) Host {
	t.Helper()
	return Host{
		Start: ci.StartInProcess, Ready: ci.WaitReady,
		TempDir: func() (string, error) { return os.MkdirTemp(t.TempDir(), "bench-*") },
		Remove:  os.RemoveAll,
		Fixture: tinyFixture(),
	}
}

// Every strategy must really isolate tests: Run verifies the fixture after
// each isolation, so a leak fails here.
func TestEveryStrategyIsolatesAgainstRealStratus(t *testing.T) {
	ctx := context.Background()
	for _, s := range Strategies(realHost(t)) {
		t.Run(s.Name(), func(t *testing.T) {
			r, err := Run(ctx, s, tinyFixture(), 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Samples) != 3 || r.Setup <= 0 {
				t.Fatalf("result: %+v", r)
			}
		})
	}
}

// noop pretends to isolate but leaves state alone.
type noop struct{ endpoint string }

func (n noop) Name() string                            { return "noop" }
func (n noop) Description() string                     { return "does nothing" }
func (n noop) Setup(context.Context) (string, error)   { return n.endpoint, nil }
func (n noop) Isolate(context.Context) (string, error) { return n.endpoint, nil }
func (n noop) Close() error                            { return nil }

func TestRunRejectsAStrategyThatLeaksState(t *testing.T) {
	ctx := context.Background()
	h := realHost(t)
	dir, _ := h.TempDir()
	srv, err := h.Start(ctx, ci.ServerConfig{DataDir: dir, LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if err := h.Ready(ctx, srv.Endpoint()); err != nil {
		t.Fatal(err)
	}
	if err := h.Fixture.Seed(ctx, AWS{Endpoint: srv.Endpoint()}); err != nil {
		t.Fatal(err)
	}

	_, err = Run(ctx, noop{endpoint: srv.Endpoint()}, h.Fixture, 1)
	if err == nil || !strings.Contains(err.Error(), "state leaked after test 0") {
		t.Fatalf("want a leak error, got %v", err)
	}
}
