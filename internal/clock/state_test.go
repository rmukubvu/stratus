package clock

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

func TestTheZeroStateRunsWithRealTime(t *testing.T) {
	if got := (State{}).Now(t0); !got.Equal(t0) {
		t.Fatalf("Now = %v, want %v", got, t0)
	}
}

func TestAdvancingARunningClockKeepsItRunningAhead(t *testing.T) {
	s, err := State{}.Advance(t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Now(t0.Add(time.Minute)); !got.Equal(t0.Add(time.Hour + time.Minute)) {
		t.Fatalf("Now = %v, want an hour ahead of real time", got)
	}
}

func TestTheClockNeverMovesBackwardsOrStandsStill(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := (State{}).Advance(t0, d); !errors.Is(err, ErrNotForward) {
			t.Errorf("Advance(%v) err = %v, want ErrNotForward", d, err)
		}
	}
	if _, err := (State{}).AdvanceTo(t0, t0.Add(-time.Minute)); !errors.Is(err, ErrNotForward) {
		t.Errorf("AdvanceTo the past err = %v, want ErrNotForward", err)
	}
}

func TestAdvanceToLandsExactlyOnTheInstant(t *testing.T) {
	to := t0.Add(36 * time.Hour)
	s, err := State{}.AdvanceTo(t0, to)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Now(t0); !got.Equal(to) {
		t.Fatalf("Now = %v, want %v", got, to)
	}
}

func TestAFrozenClockStandsStillWhileRealTimePasses(t *testing.T) {
	s := State{}.Freeze(t0)
	if got := s.Now(t0.Add(time.Hour)); !got.Equal(t0) {
		t.Fatalf("Now = %v, want it held at %v", got, t0)
	}
}

func TestAdvancingAFrozenClockMovesItAndKeepsItFrozen(t *testing.T) {
	s, err := State{}.Freeze(t0).Advance(t0, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Frozen {
		t.Fatal("advancing unfroze the clock")
	}
	if got := s.Now(t0.Add(time.Hour)); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("Now = %v, want %v", got, t0.Add(10*time.Minute))
	}
}

func TestResumeContinuesFromWhereTheClockWasHeld(t *testing.T) {
	held := State{}.Freeze(t0)
	later := t0.Add(time.Hour)
	s := held.Resume(later)
	if s.Frozen {
		t.Fatal("still frozen after resume")
	}
	if got := s.Now(later); !got.Equal(t0) {
		t.Fatalf("Now right after resume = %v, want %v (no time passes while frozen)", got, t0)
	}
	if got := s.Now(later.Add(time.Second)); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("Now = %v, want the clock running again", got)
	}
}

func TestFreezeAndResumeAreIdempotent(t *testing.T) {
	frozen := State{}.Freeze(t0)
	if again := frozen.Freeze(t0.Add(time.Hour)); again != frozen {
		t.Fatalf("freezing twice moved the clock: %+v", again)
	}
	if running := (State{Offset: time.Minute}).Resume(t0); running != (State{Offset: time.Minute}) {
		t.Fatalf("resuming a running clock changed it: %+v", running)
	}
}
