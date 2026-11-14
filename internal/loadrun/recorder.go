package loadrun

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/limits"
)

// LimitsController applies and clears a limits profile; *limits.Governor
// fits.
type LimitsController interface {
	Apply(spec limits.Spec) (limits.Status, error)
	Status() (limits.Status, error)
	Clear() (limits.Status, error)
}

// ChaosController starts and stops a chaos experiment; *chaos.Engine fits.
type ChaosController interface {
	Start(spec chaos.Spec) (chaos.Report, error)
	Report() (chaos.Report, error)
	StopID(id string) (chaos.Report, error)
}

// Clock lets tests control time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// StartInput describes a run to start.
type StartInput struct {
	// Name labels the run, e.g. the k6 script; optional.
	Name string `json:"name,omitempty"`
	// Limits, when set, is applied for the run and cleared when it stops.
	Limits *limits.Spec `json:"limits,omitempty"`
	// Chaos, when set, is started for the run and stopped when it stops.
	Chaos *chaos.Spec `json:"chaos,omitempty"`
}

// Status says whether a run is still recording.
type Status string

const (
	StatusRunning  Status = "running"
	StatusFinished Status = "finished"
)

// Summary describes a run.
type Summary struct {
	ID              string           `json:"id"`
	Name            string           `json:"name,omitempty"`
	Status          Status           `json:"status"`
	StartedAt       time.Time        `json:"started_at"`
	EndedAt         *time.Time       `json:"ended_at,omitempty"`
	DurationSeconds float64          `json:"duration_seconds"`
	Totals          Totals           `json:"totals"`
	Timeline        []Second         `json:"timeline"`
	Operations      []OperationStats `json:"operations"`
	// Limits is the limits profile's status: live while the run is
	// running, final once the run stopped and cleared it.
	Limits *limits.Status `json:"limits,omitempty"`
	// Chaos is the chaos experiment's report, likewise.
	Chaos *chaos.Report `json:"chaos,omitempty"`
}

// run is one recording window.
type run struct {
	id, name  string
	startedAt time.Time
	endedAt   time.Time
	tally     *tally
	// ownsLimits and chaosID record what the run set up, to undo it.
	ownsLimits bool
	chaosID    string
	limits     *limits.Status
	chaos      *chaos.Report
}

// keepFinished is how many finished runs stay available.
const keepFinished = 20

// Recorder holds the active run and recent finished ones. At most one run
// records at a time. It is safe for concurrent use, and Observe costs one
// atomic load while no run is active.
type Recorder struct {
	limits LimitsController
	chaos  ChaosController
	clock  Clock

	active atomic.Pointer[run]

	mu       sync.Mutex
	finished []*run
	next     int
}

// NewRecorder builds a recorder. Either controller may be nil, in which
// case runs cannot set that feature up. A nil clock uses the system clock.
func NewRecorder(l LimitsController, c ChaosController, clock Clock) *Recorder {
	if clock == nil {
		clock = systemClock{}
	}
	return &Recorder{limits: l, chaos: c, clock: clock}
}

// Observe counts a request against the active run, if any.
func (r *Recorder) Observe(o Observation) {
	if r.active.Load() == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if run := r.active.Load(); run != nil {
		run.tally.add(o)
	}
}

// Start begins a run, setting up its limits profile and chaos experiment.
// If either fails, nothing is left running.
func (r *Recorder) Start(in StartInput) (Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.active.Load(); cur != nil {
		return Summary{}, &control.Error{
			Kind: control.KindConflict, Code: "RunInProgress",
			Message: fmt.Sprintf("run %s is still recording", cur.id),
			Hint:    "stop it with POST /_stratus/control/v1/runs/" + cur.id + "/stop",
		}
	}
	if in.Limits != nil && r.limits == nil || in.Chaos != nil && r.chaos == nil {
		return Summary{}, &control.Error{Kind: control.KindUnavailable, Code: "FeatureUnavailable", Message: "limits or chaos is not available on this server"}
	}
	now := r.clock.Now()
	r.next++
	ru := &run{id: fmt.Sprintf("run-%d", r.next), name: strings.TrimSpace(in.Name), startedAt: now, tally: newTally(now)}

	if in.Limits != nil {
		status, err := r.limits.Apply(*in.Limits)
		if err != nil {
			return Summary{}, err
		}
		ru.ownsLimits, ru.limits = true, &status
	}
	if in.Chaos != nil {
		report, err := r.chaos.Start(*in.Chaos)
		if err != nil {
			if ru.ownsLimits {
				_, _ = r.limits.Clear()
			}
			return Summary{}, err
		}
		ru.chaosID, ru.chaos = report.ID, &report
	}
	r.active.Store(ru)
	return ru.summary(now), nil
}

// Stop ends the run with the given ID ("" or "current" for the active
// one), undoing what it set up, and returns its final summary.
func (r *Recorder) Stop(id string) (Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru := r.active.Load()
	if ru == nil || (id != "" && id != "current" && id != ru.id) {
		if done := r.findFinished(id); done != nil {
			return done.summary(done.endedAt), nil
		}
		return Summary{}, noRun(id)
	}
	now := r.clock.Now()
	ru.endedAt = now
	if ru.ownsLimits {
		// Another caller may have cleared or replaced the profile; the run
		// keeps the last status it saw then.
		if status, err := r.limits.Clear(); err == nil {
			ru.limits = &status
		}
	}
	if ru.chaosID != "" {
		if report, err := r.chaos.StopID(ru.chaosID); err == nil {
			ru.chaos = &report
		}
	}
	r.active.Store(nil)
	r.finished = append(r.finished, ru)
	if len(r.finished) > keepFinished {
		r.finished = r.finished[len(r.finished)-keepFinished:]
	}
	return ru.summary(now), nil
}

// Get describes a run by ID ("" or "current" for the active one).
func (r *Recorder) Get(id string) (Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ru := r.active.Load(); ru != nil && (id == "" || id == "current" || id == ru.id) {
		r.refresh(ru)
		return ru.summary(r.clock.Now()), nil
	}
	if done := r.findFinished(id); done != nil {
		return done.summary(done.endedAt), nil
	}
	return Summary{}, noRun(id)
}

// RunInfo is a run in a listing, without its timeline.
type RunInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	Status    Status     `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Requests  int        `json:"requests"`
}

// List returns the active run and the finished ones, newest first.
func (r *Recorder) List() []RunInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RunInfo
	if ru := r.active.Load(); ru != nil {
		out = append(out, ru.info())
	}
	for i := len(r.finished) - 1; i >= 0; i-- {
		out = append(out, r.finished[i].info())
	}
	return out
}

// refresh updates a running run's view of the limits and chaos it set up.
func (r *Recorder) refresh(ru *run) {
	if ru.ownsLimits {
		if status, err := r.limits.Status(); err == nil {
			ru.limits = &status
		}
	}
	if ru.chaosID != "" {
		if report, err := r.chaos.Report(); err == nil && report.ID == ru.chaosID {
			ru.chaos = &report
		}
	}
}

func (r *Recorder) findFinished(id string) *run {
	for i := len(r.finished) - 1; i >= 0; i-- {
		if id == "" || id == "latest" || r.finished[i].id == id {
			return r.finished[i]
		}
	}
	return nil
}

func (ru *run) status() Status {
	if ru.endedAt.IsZero() {
		return StatusRunning
	}
	return StatusFinished
}

func (ru *run) info() RunInfo {
	info := RunInfo{ID: ru.id, Name: ru.name, Status: ru.status(), StartedAt: ru.startedAt.UTC(), Requests: ru.tally.total.requests}
	if !ru.endedAt.IsZero() {
		end := ru.endedAt.UTC()
		info.EndedAt = &end
	}
	return info
}

func (ru *run) summary(now time.Time) Summary {
	s := Summary{
		ID: ru.id, Name: ru.name, Status: ru.status(), StartedAt: ru.startedAt.UTC(),
		DurationSeconds: round(now.Sub(ru.startedAt).Seconds()),
		Totals:          ru.tally.totals(now), Timeline: ru.tally.seconds(), Operations: ru.tally.operationStats(),
		Limits: ru.limits, Chaos: ru.chaos,
	}
	if !ru.endedAt.IsZero() {
		end := ru.endedAt.UTC()
		s.EndedAt = &end
	}
	return s
}

func noRun(id string) *control.Error {
	msg := "no load run is recording"
	if id != "" && id != "current" && id != "latest" {
		msg = fmt.Sprintf("no load run %q", id)
	}
	return &control.Error{Kind: control.KindNotFound, Code: "NoSuchRun", Message: msg, Hint: "start one with POST /_stratus/control/v1/runs"}
}
