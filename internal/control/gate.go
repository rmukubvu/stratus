package control

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// DefaultDrainTimeout bounds how long a control operation waits for
// in-flight API requests to finish before giving up.
const DefaultDrainTimeout = 30 * time.Second

// Gate keeps state-changing control operations (reset, snapshot, restore)
// from interleaving with in-flight AWS API requests.
//
// The gate prefers requests: while a control operation waits for in-flight
// requests to drain, new requests are still admitted. That matters because
// a request can fan out into nested requests (a Lambda invoked by one call
// commonly calls back into Stratus); blocking those would deadlock. Once no
// requests are in flight the operation runs and new requests wait for it.
//
// Background work started outside a request, such as asynchronous Lambda
// invocations, is not tracked and may observe the change.
type Gate struct {
	mu           sync.Mutex
	cond         *sync.Cond
	active       int
	exclusive    bool
	DrainTimeout time.Duration
}

func (g *Gate) init() {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
}

// Admit registers an in-flight request and returns the function that ends it.
func (g *Gate) Admit() (release func()) {
	g.mu.Lock()
	g.init()
	for g.exclusive {
		g.cond.Wait()
	}
	g.active++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			if g.active == 0 {
				g.cond.Broadcast()
			}
			g.mu.Unlock()
		})
	}
}

// Exclusive waits until no request is in flight, then runs fn while new
// requests are held back. It fails with a Busy error if requests do not
// drain before the context ends or the drain timeout passes.
func (g *Gate) Exclusive(ctx context.Context, fn func() error) error {
	timeout := g.DrainTimeout
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	g.mu.Lock()
	g.init()
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()

	for g.exclusive || g.active > 0 {
		if ctx.Err() != nil {
			inFlight := g.active
			g.mu.Unlock()
			return busyError(inFlight)
		}
		g.cond.Wait()
	}
	g.exclusive = true
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		g.exclusive = false
		g.cond.Broadcast()
		g.mu.Unlock()
	}()
	return fn()
}

func busyError(inFlight int) *Error {
	return &Error{
		Kind:    KindUnavailable,
		Code:    "Busy",
		Message: "timed out waiting for in-flight API requests to finish",
		Hint:    "retry once current requests complete; requests in flight: " + strconv.Itoa(inFlight),
	}
}
