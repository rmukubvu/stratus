package fis

import "time"

// experimentRecord is what Stratus stores for an experiment: the template
// snapshot it ran, the chaos run it maps to and its schedule. The status is
// derived from these and the clock, and frozen once it is final.
type experimentRecord struct {
	Experiment Experiment `json:"experiment"`
	// ChaosID is the chaos engine's ID for this run.
	ChaosID string `json:"chaos_id"`
	Plan    plan   `json:"plan"`
	// Final is set once the experiment has ended, with the time it ended.
	Final   *State `json:"final,omitempty"`
	EndedAt *Epoch `json:"ended_at,omitempty"`
}

const (
	reasonRunning   = "Experiment is running."
	reasonCompleted = "Experiment completed."
	reasonStopped   = "Experiment stopped by StopExperiment."
	reasonReplaced  = "Experiment stopped because another chaos experiment replaced it, it was stopped through the Stratus control API, or Stratus restarted."
)

// settle freezes the status of an experiment that has ended. running says
// whether its chaos run is still the engine's current one. It reports
// whether the record changed and must be saved.
func (r *experimentRecord) settle(now time.Time, running bool) bool {
	if r.Final != nil {
		return false
	}
	start := r.Experiment.StartTime.Time()
	switch {
	case r.Plan.Total > 0 && !now.Before(start.Add(r.Plan.Total)):
		r.end(State{Status: StatusCompleted, Reason: reasonCompleted}, start.Add(r.Plan.Total))
	case !running:
		r.end(State{Status: StatusStopped, Reason: reasonReplaced}, now)
	default:
		return false
	}
	return true
}

func (r *experimentRecord) end(state State, at time.Time) {
	r.Final = &state
	r.EndedAt = epochPtr(at)
}

// view is the experiment as FIS reports it at now.
func (r experimentRecord) view(now time.Time) Experiment {
	e := r.Experiment
	start := e.StartTime.Time()
	clock := now
	e.State = State{Status: StatusRunning, Reason: reasonRunning}
	if r.Final != nil {
		e.State = *r.Final
		e.EndTime = r.EndedAt
		clock = r.EndedAt.Time()
	}

	actions := make(map[string]ExperimentAction, len(e.Actions))
	for name, a := range e.Actions {
		w := r.Plan.Windows[name]
		from := start.Add(w.Start)
		a.State = actionState(w, start, clock, r.Final)
		if a.State.Status != StatusPending && a.State.Status != StatusCancelled {
			a.StartTime = epochPtr(from)
		}
		if a.State.Status == StatusCompleted || a.State.Status == StatusStopped {
			to := clock
			if !w.openEnded() && start.Add(w.end()).Before(to) {
				to = start.Add(w.end())
			}
			a.EndTime = epochPtr(to)
		}
		actions[name] = a
	}
	e.Actions = actions
	return e
}

// actionState is an action's status at clock, which is now for a running
// experiment and the end time for one that has ended.
func actionState(w window, start, clock time.Time, final *State) State {
	from := start.Add(w.Start)
	finished := !w.openEnded() && !clock.Before(start.Add(w.end()))
	switch {
	case finished:
		return State{Status: StatusCompleted, Reason: "Action completed."}
	case clock.Before(from):
		if final != nil {
			return State{Status: StatusCancelled, Reason: "Experiment ended before the action started."}
		}
		return State{Status: StatusPending, Reason: "Waiting for the actions it starts after."}
	case final != nil && final.Status == StatusCompleted:
		return State{Status: StatusCompleted, Reason: "Action completed."}
	case final != nil:
		return State{Status: StatusStopped, Reason: "Experiment stopped."}
	default:
		return State{Status: StatusRunning, Reason: "Action is running."}
	}
}
