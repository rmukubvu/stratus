package clock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) Get(bucket, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[bucket+"/"+key], nil
}

func (m *memStore) Put(bucket, key string, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[bucket+"/"+key] = val
	return nil
}

func (m *memStore) wipe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = map[string][]byte{}
}

type realTime struct{ t time.Time }

func (r *realTime) now() time.Time { return r.t }

func newClock() (*Clock, *memStore, *realTime) {
	store, real := newMemStore(), &realTime{t: t0}
	return New(store, real.now), store, real
}

func TestAnUntouchedClockReportsRealTime(t *testing.T) {
	c, _, _ := newClock()
	st := c.Status()
	if !st.Now.Equal(t0) || st.Frozen || st.Offset != "0s" {
		t.Fatalf("Status = %+v, want real time, running, no offset", st)
	}
}

func TestAdvanceMovesEveryReaderAndRunsListeners(t *testing.T) {
	c, _, _ := newClock()
	var seen time.Time
	c.OnAdvance(func(context.Context) (Effect, error) {
		seen = c.Now()
		return Effect{Source: "scheduler", Count: 2}, nil
	})
	c.OnAdvance(func(context.Context) (Effect, error) { return Effect{Source: "idle"}, nil })

	res, err := c.Advance(context.Background(), 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := t0.Add(90 * time.Minute)
	if !c.Now().Equal(want) || !res.Now.Equal(want) {
		t.Fatalf("Now = %v, result %v, want %v", c.Now(), res.Now, want)
	}
	if !seen.Equal(want) {
		t.Fatalf("listener saw %v, want the moved clock %v", seen, want)
	}
	if len(res.Effects) != 1 || res.Effects[0].Source != "scheduler" {
		t.Fatalf("Effects = %+v, want only the listener that did something", res.Effects)
	}
}

func TestARefusedMoveRunsNoListener(t *testing.T) {
	c, _, _ := newClock()
	called := false
	c.OnAdvance(func(context.Context) (Effect, error) { called = true; return Effect{}, nil })
	if _, err := c.Advance(context.Background(), -time.Minute); !errors.Is(err, ErrNotForward) {
		t.Fatalf("err = %v, want ErrNotForward", err)
	}
	if called {
		t.Fatal("a listener ran for a move that did not happen")
	}
}

func TestAFailingListenerIsReported(t *testing.T) {
	c, _, _ := newClock()
	c.OnAdvance(func(context.Context) (Effect, error) { return Effect{}, errors.New("delivery failed") })
	if _, err := c.Advance(context.Background(), time.Minute); err == nil {
		t.Fatal("a listener's failure was swallowed")
	}
}

func TestFreezeHoldsTheClockAcrossRealTimeUntilResumed(t *testing.T) {
	c, _, real := newClock()
	if _, err := c.Freeze(); err != nil {
		t.Fatal(err)
	}
	real.t = t0.Add(time.Hour)
	if st := c.Status(); !st.Now.Equal(t0) || !st.Frozen {
		t.Fatalf("Status = %+v, want frozen at %v", st, t0)
	}
	if _, err := c.Resume(); err != nil {
		t.Fatal(err)
	}
	real.t = t0.Add(time.Hour + time.Second)
	if got := c.Now(); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("Now = %v, want running again from %v", got, t0)
	}
}

func TestWipingTheStoreReturnsTheClockToRealTime(t *testing.T) {
	c, store, _ := newClock()
	if _, err := c.Advance(context.Background(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	store.wipe() // what a full reset does
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now after reset = %v, want real time %v", got, t0)
	}
}

func TestAnUnreadableRecordIsTreatedAsRealTime(t *testing.T) {
	c, store, _ := newClock()
	_ = store.Put(Bucket, key, []byte("not json"))
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now = %v, want real time", got)
	}
}
