package fis

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/chaos"
)

// compiled is a template turned into something the chaos engine can run.
type compiled struct {
	spec chaos.Spec
	plan plan
}

// compile validates a template against the action catalog and turns it
// into a chaos spec. Every supported action becomes one or more rules,
// active during the action's window.
func compile(t Template, catalog map[string]ActionDefinition, seed int64) (compiled, error) {
	if len(t.Actions) == 0 {
		return compiled{}, validation("an experiment template needs at least one action")
	}
	selected := make(map[string][]string, len(t.Targets))
	for _, name := range sortedKeys(t.Targets) {
		resources, err := selectTargets(name, t.Targets[name])
		if err != nil {
			return compiled{}, err
		}
		selected[name] = resources
	}

	for _, name := range sortedKeys(t.Actions) {
		if err := checkAction(name, t.Actions[name], t.Targets, catalog); err != nil {
			return compiled{}, err
		}
	}
	p, err := schedule(t.Actions, func(a Action) (time.Duration, error) {
		return params(a.Parameters).duration()
	})
	if err != nil {
		return compiled{}, err
	}

	spec := chaos.Spec{Seed: seed, DurationSeconds: ceilSeconds(p.Total)}
	for _, name := range sortedKeys(t.Actions) {
		a := t.Actions[name]
		byKey := make(map[string][]string, len(a.Targets))
		for key, target := range a.Targets {
			byKey[key] = selected[target]
		}
		rules, err := catalog[a.ActionID].rules(params(a.Parameters), byKey)
		if err != nil {
			return compiled{}, validation("action %q: %s", name, err.Error())
		}
		w := p.Windows[name]
		for i, r := range rules {
			r.ID = name
			if len(rules) > 1 {
				r.ID = fmt.Sprintf("%s/%d", name, i+1)
			}
			r.StartAfterSeconds = ceilSeconds(w.Start)
			r.DurationSeconds = ceilSeconds(w.Length)
			spec.Rules = append(spec.Rules, r)
		}
	}
	if len(spec.Rules) == 0 {
		return compiled{}, validation("the template has no fault actions, only waits; add an action that injects a fault")
	}
	return compiled{spec: spec, plan: p}, nil
}

func checkAction(name string, a Action, targets map[string]Target, catalog map[string]ActionDefinition) error {
	def, ok := catalog[a.ActionID]
	if !ok {
		return validation("action %q: %q is not an action Stratus supports; ListActions shows the supported actions", name, a.ActionID)
	}
	for key := range a.Parameters {
		if _, ok := def.Parameters[key]; !ok {
			return validation("action %q: %s has no parameter %q", name, a.ActionID, key)
		}
	}
	for key, p := range def.Parameters {
		if p.Required && strings.TrimSpace(a.Parameters[key]) == "" {
			return validation("action %q: parameter %q is required", name, key)
		}
	}
	for key, targetName := range a.Targets {
		want, ok := def.Targets[key]
		if !ok {
			return validation("action %q: %s has no target %q", name, a.ActionID, key)
		}
		target, ok := targets[targetName]
		if !ok {
			return validation("action %q: target %q is not defined in the template", name, targetName)
		}
		if target.ResourceType != want.ResourceType {
			return validation("action %q: target %q is %s, but %s needs %s", name, targetName, target.ResourceType, a.ActionID, want.ResourceType)
		}
	}
	return nil
}

var selectionMode = regexp.MustCompile(`^(ALL|COUNT\((\d+)\)|PERCENT\((\d+)\))$`)

// selectTargets resolves a target to the resources it selects; nil means
// every resource of its type.
func selectTargets(name string, t Target) ([]string, error) {
	mode := t.SelectionMode
	if mode == "" {
		mode = "ALL"
	}
	m := selectionMode.FindStringSubmatch(mode)
	if m == nil {
		return nil, validation("target %q: selectionMode must be ALL, COUNT(n) or PERCENT(n)", name)
	}

	switch t.ResourceType {
	case ResourceIAMRole:
		return nil, nil
	case ResourceLambdaFunction:
	default:
		return nil, validation("target %q: resource type %q is not supported by Stratus; ListTargetResourceTypes shows the supported types", name, t.ResourceType)
	}
	if len(t.ResourceTags) > 0 || len(t.Filters) > 0 {
		return nil, validation("target %q: Stratus selects Lambda functions by resourceArns only, not by resourceTags or filters", name)
	}
	if len(t.ResourceArns) == 0 {
		if m[1] != "ALL" {
			return nil, validation("target %q: COUNT and PERCENT selection need resourceArns", name)
		}
		return nil, nil
	}

	functions := make([]string, 0, len(t.ResourceArns))
	for _, arn := range t.ResourceArns {
		functions = append(functions, functionName(arn))
	}
	sort.Strings(functions)
	n := len(functions)
	switch {
	case m[2] != "":
		n, _ = strconv.Atoi(m[2])
	case m[3] != "":
		pct, _ := strconv.Atoi(m[3])
		n = int(math.Ceil(float64(len(functions)) * float64(pct) / 100))
	}
	// Selection is deterministic: the first n functions by name.
	return functions[:min(n, len(functions))], nil
}

// functionName takes a function name out of a Lambda ARN, dropping any
// version or alias qualifier.
func functionName(arn string) string {
	_, rest, ok := strings.Cut(arn, ":function:")
	if !ok {
		return arn
	}
	name, _, _ := strings.Cut(rest, ":")
	return name
}

func ceilSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}
