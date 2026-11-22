// Package clock is the one clock every Stratus-owned behaviour reads time
// from: message visibility and delays, alarm evaluation periods, schedule
// firing, and every timestamp a service records.
//
// It runs with real time until a test moves it. Advancing it carries the
// whole emulator across a wait (an SQS visibility timeout, an alarm's
// evaluation period, a nightly schedule) without sleeping; freezing it holds
// a moment still so a test can look at it. It never moves backwards, so
// timestamps a service already recorded stay in order.
//
// The clock governs what Stratus itself decides. Real engines in containers
// (Lambda runtimes, Postgres, Redis, Redpanda) keep their own system time.
package clock

import (
	"errors"
	"time"
)

// ErrNotForward is returned for a move that would not take the clock forward.
var ErrNotForward = errors.New("the clock only moves forward")

// State is the clock's setting relative to real time. The zero value is a
// clock that runs with real time.
type State struct {
	// Offset is how far ahead of real time the clock runs while it runs.
	Offset time.Duration `json:"offset_ns"`
	// Frozen holds the clock at FrozenAt until it is resumed.
	Frozen   bool      `json:"frozen,omitempty"`
	FrozenAt time.Time `json:"frozen_at,omitempty"`
}

// Now is the clock's time when real time is real.
func (s State) Now(real time.Time) time.Time {
	if s.Frozen {
		return s.FrozenAt.UTC()
	}
	return real.Add(s.Offset).UTC()
}

// Advance moves the clock forward by d, frozen or not.
func (s State) Advance(real time.Time, d time.Duration) (State, error) {
	if d <= 0 {
		return s, ErrNotForward
	}
	if s.Frozen {
		s.FrozenAt = s.FrozenAt.Add(d)
	} else {
		s.Offset += d
	}
	return s, nil
}

// AdvanceTo moves the clock to the instant to, which must be ahead of it.
func (s State) AdvanceTo(real, to time.Time) (State, error) {
	return s.Advance(real, to.Sub(s.Now(real)))
}

// Freeze holds the clock where it stands. Freezing a frozen clock changes
// nothing.
func (s State) Freeze(real time.Time) State {
	if s.Frozen {
		return s
	}
	return State{Offset: s.Offset, Frozen: true, FrozenAt: s.Now(real)}
}

// Resume lets a frozen clock run again from where it was held, so no time
// passes on the clock for the time it spent frozen.
func (s State) Resume(real time.Time) State {
	if !s.Frozen {
		return s
	}
	return State{Offset: s.FrozenAt.Sub(real)}
}
