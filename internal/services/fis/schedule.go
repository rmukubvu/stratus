package fis

import (
	"regexp"
	"sort"
	"strconv"
	"time"
)

// window is when an action runs, relative to the experiment start.
type window struct {
	Start time.Duration `json:"start"`
	// Length is zero for an action without a duration, which runs until
	// the experiment is stopped.
	Length time.Duration `json:"length"`
}

func (w window) openEnded() bool { return w.Length == 0 }

func (w window) end() time.Duration { return w.Start + w.Length }

// plan is when each action runs. Total is zero when an action is
// open-ended, so the experiment runs until it is stopped.
type plan struct {
	Windows map[string]window `json:"windows"`
	Total   time.Duration     `json:"total"`
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// parseDuration reads an ISO 8601 duration such as PT5M or PT1H30M, the
// format FIS uses for its duration parameters.
func parseDuration(s string) (time.Duration, bool) {
	m := isoDuration.FindStringSubmatch(s)
	if m == nil || s == "P" || s == "PT" {
		return 0, false
	}
	var d time.Duration
	units := []time.Duration{24 * time.Hour, time.Hour, time.Minute}
	for i, unit := range units {
		if m[i+1] != "" {
			n, _ := strconv.Atoi(m[i+1])
			d += time.Duration(n) * unit
		}
	}
	if m[4] != "" {
		secs, _ := strconv.ParseFloat(m[4], 64)
		d += time.Duration(secs * float64(time.Second))
	}
	return d, d > 0
}

// schedule orders actions by their startAfter dependencies: an action
// starts once every action it depends on has finished.
func schedule(actions map[string]Action, durationOf func(Action) (time.Duration, error)) (plan, error) {
	lengths := make(map[string]time.Duration, len(actions))
	for _, name := range sortedKeys(actions) {
		a := actions[name]
		for _, dep := range a.StartAfter {
			if _, ok := actions[dep]; !ok {
				return plan{}, validation("action %q starts after %q, which is not an action in this template", name, dep)
			}
		}
		d, err := durationOf(a)
		if err != nil {
			return plan{}, validation("action %q: %s", name, err.Error())
		}
		lengths[name] = d
	}

	p := plan{Windows: make(map[string]window, len(actions))}
	visiting := map[string]bool{}
	var place func(name string) error
	place = func(name string) error {
		if _, done := p.Windows[name]; done {
			return nil
		}
		if visiting[name] {
			return validation("actions have a startAfter cycle through %q", name)
		}
		visiting[name] = true
		var start time.Duration
		for _, dep := range actions[name].StartAfter {
			if err := place(dep); err != nil {
				return err
			}
			w := p.Windows[dep]
			if w.openEnded() {
				return validation("action %q starts after %q, which has no duration and never finishes", name, dep)
			}
			start = max(start, w.end())
		}
		visiting[name] = false
		p.Windows[name] = window{Start: start, Length: lengths[name]}
		return nil
	}

	openEnded := false
	for _, name := range sortedKeys(actions) {
		if err := place(name); err != nil {
			return plan{}, err
		}
		w := p.Windows[name]
		if w.openEnded() {
			openEnded = true
		}
		p.Total = max(p.Total, w.end())
	}
	if openEnded {
		p.Total = 0
	}
	return p, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
