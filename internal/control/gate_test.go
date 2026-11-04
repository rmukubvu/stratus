package control

import (
	"context"
	"testing"
	"time"
)

func TestExclusiveWaitsForInFlightRequests(t *testing.T) {
	g := &Gate{}
	release := g.Admit()

	done := make(chan struct{})
	go func() {
		_ = g.Exclusive(context.Background(), func() error { return nil })
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("exclusive ran while a request was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive did not run after the request finished")
	}
}

// A request that triggers nested requests (Lambda calling back into Stratus)
// must not deadlock while a control operation is waiting to run.
func TestNestedRequestsAreAdmittedWhileExclusiveWaits(t *testing.T) {
	g := &Gate{}
	outer := g.Admit()

	ran := make(chan struct{})
	go func() {
		_ = g.Exclusive(context.Background(), func() error { close(ran); return nil })
	}()
	time.Sleep(20 * time.Millisecond) // let Exclusive start waiting

	admitted := make(chan func())
	go func() { admitted <- g.Admit() }()
	select {
	case inner := <-admitted:
		inner()
	case <-time.After(time.Second):
		t.Fatal("nested request blocked behind a waiting control operation")
	}
	outer()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive never ran")
	}
}

func TestRequestsWaitWhileExclusiveRuns(t *testing.T) {
	g := &Gate{}
	inside := make(chan struct{})
	finish := make(chan struct{})
	go func() {
		_ = g.Exclusive(context.Background(), func() error {
			close(inside)
			<-finish
			return nil
		})
	}()
	<-inside

	admitted := make(chan struct{})
	go func() { g.Admit()(); close(admitted) }()
	select {
	case <-admitted:
		t.Fatal("request admitted during exclusive operation")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("request not admitted after exclusive finished")
	}
}

func TestExclusiveReturnsBusyWhenRequestsDoNotDrain(t *testing.T) {
	g := &Gate{DrainTimeout: 30 * time.Millisecond}
	release := g.Admit()
	defer release()

	called := false
	err := g.Exclusive(context.Background(), func() error { called = true; return nil })
	requireCode(t, err, "Busy")
	if called {
		t.Fatal("fn ran despite timeout")
	}
	// The gate must still work afterwards.
	release()
	if err := g.Exclusive(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("gate unusable after timeout: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g := &Gate{}
	release := g.Admit()
	release()
	release()
	if g.active != 0 {
		t.Fatalf("active = %d after double release", g.active)
	}
}
