// Package idle stops a Stratus nobody is using.
//
// An agent or a script that starts `stratus serve` in the background and then
// forgets it leaves a process, a port and engine containers behind. A Watcher
// sees every request; once none has arrived for the configured timeout and none
// is still in flight, it reports the server idle so the caller can shut down
// and release everything it holds.
//
// Probes do not count as use: a supervisor polling health or readiness must not
// keep an abandoned emulator alive. A long-running request (an SQS long poll, a
// slow Lambda invoke) keeps the server busy until it returns.
package idle

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Probe reports whether a request is a liveness or readiness check, which
// does not count as use.
type Probe func(*http.Request) bool

// Watcher tracks when the server was last used.
type Watcher struct {
	timeout time.Duration
	now     func() time.Time
	probe   Probe

	mu       sync.Mutex
	lastUsed time.Time
	inFlight int
}

// NewWatcher starts the idle clock now. A nil now uses the wall clock; a nil
// probe counts every request as use.
func NewWatcher(timeout time.Duration, now func() time.Time, probe Probe) *Watcher {
	if now == nil {
		now = time.Now
	}
	if probe == nil {
		probe = func(*http.Request) bool { return false }
	}
	return &Watcher{timeout: timeout, now: now, probe: probe, lastUsed: now()}
}

// Timeout is how long the server may go unused.
func (w *Watcher) Timeout() time.Duration { return w.timeout }

// Middleware records every request that is not a probe as use, from the
// moment it arrives until its handler returns.
func (w *Watcher) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.probe(r) {
			next.ServeHTTP(rw, r)
			return
		}
		w.begin()
		defer w.end()
		next.ServeHTTP(rw, r)
	})
}

func (w *Watcher) begin() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight++
	w.lastUsed = w.now()
}

func (w *Watcher) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight--
	w.lastUsed = w.now()
}

// Idle reports whether no request is in flight and the last one finished at
// least the timeout ago.
func (w *Watcher) Idle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight == 0 && w.now().Sub(w.lastUsed) >= w.timeout
}

// Wait blocks until the server is idle, checking every interval, and returns
// true; it returns false when ctx ends first.
func (w *Watcher) Wait(ctx context.Context, interval time.Duration) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if w.Idle() {
				return true
			}
		}
	}
}

// CheckInterval is how often to check for idleness: often enough that the
// server stops close to its timeout, rarely enough to cost nothing.
func CheckInterval(timeout time.Duration) time.Duration {
	interval := timeout / 10
	switch {
	case interval < 100*time.Millisecond:
		return 100 * time.Millisecond
	case interval > 5*time.Second:
		return 5 * time.Second
	default:
		return interval
	}
}
