package container

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeRuntime starts fake containers and records cleanups.
type fakeRuntime struct {
	mu      sync.Mutex
	started int
	removed []string
	fail    bool
}

func (f *fakeRuntime) create(key string) func(context.Context, int) (*WarmContainer, error) {
	return func(_ context.Context, slot int) (*WarmContainer, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			return nil, errors.New("docker is down")
		}
		f.started++
		return &WarmContainer{ID: fmt.Sprintf("%s-%d", key, slot), Function: key}, nil
	}
}

func healthy(context.Context, *WarmContainer) (bool, error) { return true, nil }

func (f *fakeRuntime) cleanup(_ context.Context, w *WarmContainer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w != nil {
		f.removed = append(f.removed, w.ID)
	}
	return nil
}

func acquire(t *testing.T, p *WarmPool, f *fakeRuntime, key string) *WarmContainer {
	t.Helper()
	w, err := p.Acquire(context.Background(), key, f.create(key), healthy, f.cleanup)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPoolReusesAnIdleContainer(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{}
	w := acquire(t, p, f, "fn")
	p.Release(w)
	if again := acquire(t, p, f, "fn"); again != w || f.started != 1 {
		t.Fatalf("started %d containers for sequential invocations", f.started)
	}
}

func TestPoolStartsAContainerPerConcurrentInvocationUpToTheCap(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{}
	p.SetMax(2)
	a := acquire(t, p, f, "fn")
	b := acquire(t, p, f, "fn")
	if a == b || a.Slot == b.Slot || f.started != 2 {
		t.Fatalf("want two containers in distinct slots: %+v %+v", a, b)
	}

	got := make(chan *WarmContainer)
	go func() { got <- acquire(t, p, f, "fn") }()
	select {
	case <-got:
		t.Fatal("a third invocation got a container past the cap")
	case <-time.After(50 * time.Millisecond):
	}
	p.Release(a)
	select {
	case w := <-got:
		if w != a {
			t.Fatal("the waiting invocation did not get the freed container")
		}
	case <-time.After(time.Second):
		t.Fatal("releasing a container did not wake the waiter")
	}
	if s := p.Stats(); s.Containers != 2 || s.Busy != 2 || s.ColdStarts != 2 || s.Queued != 1 || s.MaxPerFunction != 2 {
		t.Fatalf("stats: %+v", s)
	}
	// Other functions have their own cap.
	acquire(t, p, f, "other")
}

func TestPoolWaitStopsWithTheContext(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{}
	p.SetMax(1)
	acquire(t, p, f, "fn")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, "fn", f.create("fn"), healthy, f.cleanup); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscardFreesTheSlot(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{}
	p.SetMax(1)
	w := acquire(t, p, f, "fn")
	if err := p.Discard(context.Background(), w, f.cleanup); err != nil {
		t.Fatal(err)
	}
	next := acquire(t, p, f, "fn")
	if next == w || next.Slot != 0 || len(f.removed) != 1 {
		t.Fatalf("discard: next %+v removed %v", next, f.removed)
	}
}

func TestFailedStartFreesTheSlot(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{fail: true}
	p.SetMax(1)
	if _, err := p.Acquire(context.Background(), "fn", f.create("fn"), healthy, f.cleanup); err == nil {
		t.Fatal("start failure not returned")
	}
	f.fail = false
	acquire(t, p, f, "fn")
}

func TestUnhealthyContainersAreReplaced(t *testing.T) {
	p, f := NewWarmPool(time.Minute), &fakeRuntime{}
	w := acquire(t, p, f, "fn")
	p.Release(w)
	sick := func(context.Context, *WarmContainer) (bool, error) { return false, nil }
	next, err := p.Acquire(context.Background(), "fn", f.create("fn"), sick, f.cleanup)
	if err != nil || next == w || len(f.removed) != 1 {
		t.Fatalf("unhealthy container kept: %v %v", err, f.removed)
	}
}

func TestExpireRemoveAndCloseAll(t *testing.T) {
	p, f := NewWarmPool(time.Nanosecond), &fakeRuntime{}
	idle := acquire(t, p, f, "fn")
	busy := acquire(t, p, f, "fn")
	p.Release(idle)
	time.Sleep(time.Millisecond)
	if err := p.ExpireIdle(context.Background(), f.cleanup); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 || f.removed[0] != idle.ID {
		t.Fatalf("expired %v, want only the idle container", f.removed)
	}
	_ = busy
	acquire(t, p, f, "other")
	if err := p.Remove(context.Background(), "fn", f.cleanup); err != nil || len(f.removed) != 2 {
		t.Fatalf("remove: %v %v", err, f.removed)
	}
	if err := p.CloseAll(context.Background(), f.cleanup); err != nil || len(f.removed) != 3 {
		t.Fatalf("close all: %v %v", err, f.removed)
	}
	if s := p.Stats(); s.Containers != 0 {
		t.Fatalf("containers left: %+v", s)
	}
}
