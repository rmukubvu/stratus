package loadrun

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/limits"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeLimits struct {
	applied, cleared int
	failApply        bool
}

func (f *fakeLimits) Apply(spec limits.Spec) (limits.Status, error) {
	if f.failApply {
		return limits.Status{}, errors.New("bad profile")
	}
	f.applied++
	return limits.Status{Profile: spec.Profile}, nil
}
func (f *fakeLimits) Status() (limits.Status, error) {
	return limits.Status{Profile: "live", TotalThrottled: 7}, nil
}
func (f *fakeLimits) Clear() (limits.Status, error) {
	f.cleared++
	return limits.Status{Profile: "final", TotalThrottled: 9}, nil
}

type fakeChaos struct {
	started, stopped int
	fail             bool
}

func (f *fakeChaos) Start(chaos.Spec) (chaos.Report, error) {
	if f.fail {
		return chaos.Report{}, errors.New("bad experiment")
	}
	f.started++
	return chaos.Report{ID: "exp-1", Seed: 42}, nil
}
func (f *fakeChaos) Report() (chaos.Report, error) {
	return chaos.Report{ID: "exp-1", TotalInjected: 3}, nil
}
func (f *fakeChaos) StopID(id string) (chaos.Report, error) {
	f.stopped++
	return chaos.Report{ID: id, TotalInjected: 5}, nil
}

func obs(t time.Time, op string, status int, code string, ms int) Observation {
	return Observation{Time: t, Service: "dynamodb", Operation: op, Status: status, Code: code, Duration: time.Duration(ms) * time.Millisecond}
}

func TestObserveIsIgnoredWithoutARun(t *testing.T) {
	r := NewRecorder(nil, nil, newClock())
	r.Observe(obs(time.Now(), "PutItem", 200, "", 1))
	if _, err := r.Get(""); err == nil {
		t.Fatal("Get with no run should fail")
	}
}

func TestRunCountsOutcomesPerSecondAndPerOperation(t *testing.T) {
	clock := newClock()
	r := NewRecorder(nil, nil, clock)
	started, err := r.Start(StartInput{Name: "checkout.js"})
	if err != nil || started.ID != "run-1" || started.Status != StatusRunning {
		t.Fatalf("start: %+v %v", started, err)
	}
	t0 := clock.Now()
	r.Observe(obs(t0, "PutItem", 200, "", 5))
	r.Observe(obs(t0.Add(100*time.Millisecond), "PutItem", 400, "ProvisionedThroughputExceededException", 2))
	r.Observe(Observation{Time: t0.Add(200 * time.Millisecond), Service: "ssm", Operation: "GetParameter", Status: 400, Code: "ThrottlingException", LimitRule: "ssm-get-parameters"})
	r.Observe(Observation{Time: t0.Add(2 * time.Second), Service: "sqs", Operation: "SendMessage", Status: 500, Code: "InternalError", Fault: true})
	r.Observe(obs(t0.Add(2500*time.Millisecond), "GetItem", 400, "ValidationException", 1))
	r.Observe(Observation{Time: t0.Add(2600 * time.Millisecond), Service: "s3", Operation: "GetObject", Status: 0, Code: "ConnectionDropped", Fault: true})
	clock.Advance(4 * time.Second)

	s, err := r.Stop("")
	if err != nil {
		t.Fatal(err)
	}
	tot := s.Totals
	if tot.Requests != 6 || tot.OK != 1 || tot.Throttled != 2 || tot.Faults != 2 || tot.Errors != 1 {
		t.Fatalf("totals: %+v", tot)
	}
	if tot.PerSecond != 1.5 || tot.ThrottleRate != 0.33 || tot.ErrorRate != 0.17 {
		t.Fatalf("rates: %+v", tot)
	}
	if len(s.Timeline) != 3 || s.Timeline[0].Requests != 3 || s.Timeline[1].Requests != 0 || s.Timeline[2].Requests != 3 {
		t.Fatalf("timeline: %+v", s.Timeline)
	}
	if s.Operations[0].Operation != "PutItem" || s.Operations[0].Requests != 2 || s.Operations[0].Throttled != 1 {
		t.Fatalf("operations: %+v", s.Operations)
	}
	if s.Status != StatusFinished || s.EndedAt == nil || s.DurationSeconds != 4 || s.Name != "checkout.js" {
		t.Fatalf("summary: %+v", s)
	}
	// Requests after the run ends are not counted.
	r.Observe(obs(clock.Now(), "PutItem", 200, "", 1))
	if again, _ := r.Get("run-1"); again.Totals.Requests != 6 {
		t.Fatal("a finished run kept counting")
	}
}

func TestOnlyOneRunRecordsAtATime(t *testing.T) {
	r := NewRecorder(nil, nil, newClock())
	_, _ = r.Start(StartInput{})
	_, err := r.Start(StartInput{})
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Code != "RunInProgress" {
		t.Fatalf("second start: %v", err)
	}
	if _, err := r.Stop("run-9"); err == nil {
		t.Fatal("stopped a run that does not exist")
	}
	_, _ = r.Stop("current")
	second, err := r.Start(StartInput{})
	if err != nil || second.ID != "run-2" {
		t.Fatalf("start after stop: %+v %v", second, err)
	}
	if list := r.List(); len(list) != 2 || list[0].ID != "run-2" || list[1].Status != StatusFinished {
		t.Fatalf("list: %+v", list)
	}
}

func TestRunSetsUpAndUndoesLimitsAndChaos(t *testing.T) {
	l, c := &fakeLimits{}, &fakeChaos{}
	r := NewRecorder(l, c, newClock())
	s, err := r.Start(StartInput{Limits: &limits.Spec{Profile: "strict"}, Chaos: &chaos.Spec{Seed: 42}})
	if err != nil || l.applied != 1 || c.started != 1 || s.Limits.Profile != "strict" || s.Chaos.Seed != 42 {
		t.Fatalf("start: %+v %v", s, err)
	}
	live, _ := r.Get("current")
	if live.Limits.TotalThrottled != 7 || live.Chaos.TotalInjected != 3 {
		t.Fatalf("live view: %+v %+v", live.Limits, live.Chaos)
	}
	final, _ := r.Stop("")
	if l.cleared != 1 || c.stopped != 1 || final.Limits.Profile != "final" || final.Chaos.TotalInjected != 5 {
		t.Fatalf("stop: %+v %+v", final.Limits, final.Chaos)
	}
}

func TestFailedChaosLeavesNothingRunning(t *testing.T) {
	l, c := &fakeLimits{}, &fakeChaos{fail: true}
	r := NewRecorder(l, c, newClock())
	if _, err := r.Start(StartInput{Limits: &limits.Spec{}, Chaos: &chaos.Spec{}}); err == nil {
		t.Fatal("start succeeded")
	}
	if l.cleared != 1 {
		t.Fatal("limits applied by a failed start were not cleared")
	}
	if _, err := r.Start(StartInput{}); err != nil {
		t.Fatalf("a failed start left a run behind: %v", err)
	}
}

func TestRunWithoutControllersRefusesLimits(t *testing.T) {
	r := NewRecorder(nil, nil, newClock())
	if _, err := r.Start(StartInput{Limits: &limits.Spec{}}); err == nil {
		t.Fatal("started limits with no controller")
	}
}

func TestHistogramPercentiles(t *testing.T) {
	var h histogram
	for i := 1; i <= 100; i++ {
		h.add(time.Duration(i) * time.Millisecond)
	}
	p50, p95 := h.percentile(0.5), h.percentile(0.95)
	if p50 < 50 || p50 > 50*histogramGrowth || p95 < 95 || p95 > 100 {
		t.Fatalf("p50 %v p95 %v", p50, p95)
	}
	if h.percentile(1) != 100 || h.max != 100 {
		t.Fatalf("max: %v %v", h.percentile(1), h.max)
	}
	var tiny histogram
	tiny.add(10 * time.Microsecond)
	if tiny.percentile(0.5) != 0.01 {
		t.Fatalf("sub-bucket value: %v", tiny.percentile(0.5))
	}
}
