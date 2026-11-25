package idle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)} }

func healthProbe(r *http.Request) bool { return r.URL.Path == "/health" }

func serve(t *testing.T, h http.Handler, path string) {
	t.Helper()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
}

func TestServerThatWasNeverUsedBecomesIdleAfterTheTimeout(t *testing.T) {
	clock := newClock()
	w := NewWatcher(time.Minute, clock.now, nil)

	clock.advance(59 * time.Second)
	if w.Idle() {
		t.Fatal("idle before the timeout")
	}
	clock.advance(time.Second)
	if !w.Idle() {
		t.Fatal("not idle at the timeout")
	}
}

func TestARequestRestartsTheIdleClock(t *testing.T) {
	clock := newClock()
	w := NewWatcher(time.Minute, clock.now, nil)
	h := w.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	clock.advance(50 * time.Second)
	serve(t, h, "/")
	clock.advance(50 * time.Second)
	if w.Idle() {
		t.Fatal("idle 50s after a request with a 1m timeout")
	}
	clock.advance(10 * time.Second)
	if !w.Idle() {
		t.Fatal("not idle a full timeout after the last request")
	}
}

func TestProbesDoNotKeepTheServerAlive(t *testing.T) {
	clock := newClock()
	w := NewWatcher(time.Minute, clock.now, healthProbe)
	h := w.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for i := 0; i < 6; i++ {
		clock.advance(10 * time.Second)
		serve(t, h, "/health")
	}
	if !w.Idle() {
		t.Fatal("health probes kept an unused server alive")
	}
}

func TestARequestStillInFlightKeepsTheServerBusy(t *testing.T) {
	clock := newClock()
	w := NewWatcher(time.Minute, clock.now, nil)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := w.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))

	go func() { serve(t, h, "/long-poll"); close(done) }()
	<-started
	clock.advance(10 * time.Minute)
	if w.Idle() {
		t.Fatal("idle while a request is in flight")
	}

	close(release)
	<-done
	if w.Idle() {
		t.Fatal("idle the instant a long request finished")
	}
	clock.advance(time.Minute)
	if !w.Idle() {
		t.Fatal("not idle a timeout after the long request finished")
	}
}

func TestWaitReturnsOnceIdle(t *testing.T) {
	w := NewWatcher(20*time.Millisecond, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !w.Wait(ctx, 5*time.Millisecond) {
		t.Fatal("Wait returned before the server went idle")
	}
}

func TestWaitStopsWhenTheContextEnds(t *testing.T) {
	w := NewWatcher(time.Hour, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w.Wait(ctx, time.Millisecond) {
		t.Fatal("Wait reported idle after its context ended")
	}
}

func TestCheckIntervalStaysWithinBounds(t *testing.T) {
	for _, tc := range []struct{ timeout, want time.Duration }{
		{time.Second, 100 * time.Millisecond},
		{30 * time.Second, 3 * time.Second},
		{time.Hour, 5 * time.Second},
	} {
		if got := CheckInterval(tc.timeout); got != tc.want {
			t.Errorf("CheckInterval(%v) = %v, want %v", tc.timeout, got, tc.want)
		}
	}
}
