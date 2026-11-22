package clock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	// Bucket is the metadata bucket the clock's state lives in. It is not
	// owned by any service, so only a full reset clears it, and every
	// snapshot carries it.
	Bucket = "stratus-clock"
	key    = "state"
)

// Store is the part of the metadata store the clock needs.
type Store interface {
	Get(bucket, key string) ([]byte, error)
	Put(bucket, key string, val []byte) error
}

// Effect is what a listener did because the clock moved, such as the
// schedules that fired.
type Effect struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
	Detail any    `json:"detail,omitempty"`
}

// Listener reacts to the clock moving forward, synchronously, so whatever
// became due has happened by the time the move returns.
type Listener func(ctx context.Context) (Effect, error)

// Status is the clock as a caller sees it.
type Status struct {
	Now      time.Time `json:"now"`
	RealNow  time.Time `json:"real_now"`
	Offset   string    `json:"offset"`
	Frozen   bool      `json:"frozen"`
	FrozenAt time.Time `json:"frozen_at,omitzero"`
}

// MoveResult is the clock after a move and what the move set off.
type MoveResult struct {
	Status
	Effects []Effect `json:"effects"`
}

// Clock is the emulator's clock. Its state lives in the metadata store, so a
// reset returns it to real time and a snapshot restore returns it to the
// time it was captured at.
type Clock struct {
	store Store
	real  func() time.Time

	mu        sync.Mutex
	listeners []Listener
}

// New builds a clock over store. A nil real uses the wall clock.
func New(store Store, real func() time.Time) *Clock {
	if real == nil {
		real = time.Now
	}
	return &Clock{store: store, real: real}
}

// OnAdvance registers a listener for forward moves.
func (c *Clock) OnAdvance(l Listener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listeners = append(c.listeners, l)
}

// Now is the emulator's current time. Services take this as their clock.
func (c *Clock) Now() time.Time {
	return c.state().Now(c.real())
}

// Status reports the clock's time and setting.
func (c *Clock) Status() Status {
	return c.status(c.state(), c.real())
}

// Advance moves the clock forward by d and runs every listener.
func (c *Clock) Advance(ctx context.Context, d time.Duration) (MoveResult, error) {
	return c.move(ctx, func(s State, real time.Time) (State, error) { return s.Advance(real, d) })
}

// AdvanceTo moves the clock forward to to and runs every listener.
func (c *Clock) AdvanceTo(ctx context.Context, to time.Time) (MoveResult, error) {
	return c.move(ctx, func(s State, real time.Time) (State, error) { return s.AdvanceTo(real, to) })
}

// Freeze holds the clock where it stands.
func (c *Clock) Freeze() (Status, error) {
	return c.set(func(s State, real time.Time) State { return s.Freeze(real) })
}

// Resume lets a frozen clock run again from where it was held.
func (c *Clock) Resume() (Status, error) {
	return c.set(func(s State, real time.Time) State { return s.Resume(real) })
}

func (c *Clock) move(ctx context.Context, step func(State, time.Time) (State, error)) (MoveResult, error) {
	c.mu.Lock()
	real := c.real()
	next, err := step(c.state(), real)
	if err == nil {
		err = c.save(next)
	}
	listeners := append([]Listener(nil), c.listeners...)
	c.mu.Unlock()
	if err != nil {
		return MoveResult{}, err
	}

	effects := []Effect{}
	for _, l := range listeners {
		effect, err := l(ctx)
		if err != nil {
			return MoveResult{}, fmt.Errorf("after moving the clock: %w", err)
		}
		if effect.Count > 0 {
			effects = append(effects, effect)
		}
	}
	return MoveResult{Status: c.Status(), Effects: effects}, nil
}

func (c *Clock) set(step func(State, time.Time) State) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	real := c.real()
	next := step(c.state(), real)
	if err := c.save(next); err != nil {
		return Status{}, err
	}
	return c.status(next, real), nil
}

func (c *Clock) status(s State, real time.Time) Status {
	st := Status{Now: s.Now(real), RealNow: real.UTC(), Offset: s.Now(real).Sub(real).Round(time.Millisecond).String(), Frozen: s.Frozen}
	if s.Frozen {
		st.FrozenAt = s.FrozenAt.UTC()
	}
	return st
}

// state reads the stored setting on every call, so a reset or a restore
// that rewrote the store is seen at once. A missing or unreadable record is
// a clock running with real time.
func (c *Clock) state() State {
	raw, err := c.store.Get(Bucket, key)
	if err != nil || raw == nil {
		return State{}
	}
	var s State
	if json.Unmarshal(raw, &s) != nil {
		return State{}
	}
	return s
}

func (c *Clock) save(s State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := c.store.Put(Bucket, key, raw); err != nil {
		return fmt.Errorf("save clock: %w", err)
	}
	return nil
}
