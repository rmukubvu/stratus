// Package fis emulates the AWS Fault Injection Service (FIS) API on top of
// Stratus's deterministic chaos engine.
//
// Experiment templates are stored as FIS stores them. Starting an
// experiment compiles the template into a chaos.Spec: each supported FIS
// action becomes one or more seeded chaos rules, active in the time window
// its startAfter dependencies and duration give it. Because the engine runs
// one experiment at a time, starting an FIS experiment replaces whatever
// chaos experiment is running, exactly as PUT /_stratus/control/v1/chaos
// does.
package fis

import (
	"encoding/json"
	"math"
	"time"
)

// Epoch is a timestamp that FIS's REST-JSON protocol encodes as epoch
// seconds.
type Epoch time.Time

// MarshalJSON writes fractional epoch seconds.
func (e Epoch) MarshalJSON() ([]byte, error) {
	return json.Marshal(float64(time.Time(e).UnixMilli()) / 1000)
}

// UnmarshalJSON reads fractional epoch seconds.
func (e *Epoch) UnmarshalJSON(b []byte) error {
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	whole, frac := math.Modf(secs)
	*e = Epoch(time.Unix(int64(whole), int64(frac*1e9)).UTC())
	return nil
}

// Time returns the timestamp as a time.Time.
func (e Epoch) Time() time.Time { return time.Time(e) }

func epochPtr(t time.Time) *Epoch {
	e := Epoch(t.UTC())
	return &e
}

// Target selects the resources an action acts on.
type Target struct {
	ResourceType  string            `json:"resourceType"`
	ResourceArns  []string          `json:"resourceArns,omitempty"`
	ResourceTags  map[string]string `json:"resourceTags,omitempty"`
	Filters       []TargetFilter    `json:"filters,omitempty"`
	SelectionMode string            `json:"selectionMode"`
	Parameters    map[string]string `json:"parameters,omitempty"`
}

// TargetFilter narrows a target by resource attributes.
type TargetFilter struct {
	Path   string   `json:"path"`
	Values []string `json:"values"`
}

// Action is one fault (or wait) in a template.
type Action struct {
	ActionID    string            `json:"actionId"`
	Description string            `json:"description,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
	Targets     map[string]string `json:"targets,omitempty"`
	StartAfter  []string          `json:"startAfter,omitempty"`
}

// StopCondition is accepted and stored; Stratus has no CloudWatch alarms
// to evaluate, so only "none" has an effect (none).
type StopCondition struct {
	Source string `json:"source"`
	Value  string `json:"value,omitempty"`
}

// Template is an experiment template.
type Template struct {
	ID             string            `json:"id"`
	Arn            string            `json:"arn"`
	Description    string            `json:"description"`
	Targets        map[string]Target `json:"targets"`
	Actions        map[string]Action `json:"actions"`
	StopConditions []StopCondition   `json:"stopConditions"`
	RoleArn        string            `json:"roleArn,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	CreationTime   Epoch             `json:"creationTime"`
	LastUpdateTime Epoch             `json:"lastUpdateTime"`
}

// TemplateSummary is a template as ListExperimentTemplates shows it.
type TemplateSummary struct {
	ID             string            `json:"id"`
	Arn            string            `json:"arn"`
	Description    string            `json:"description"`
	CreationTime   Epoch             `json:"creationTime"`
	LastUpdateTime Epoch             `json:"lastUpdateTime"`
	Tags           map[string]string `json:"tags,omitempty"`
}

func (t Template) summary() TemplateSummary {
	return TemplateSummary{
		ID: t.ID, Arn: t.Arn, Description: t.Description,
		CreationTime: t.CreationTime, LastUpdateTime: t.LastUpdateTime, Tags: t.Tags,
	}
}

// Experiment and action statuses, as FIS names them.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusStopped   = "stopped"
	StatusCancelled = "cancelled"
)

// State is the status of an experiment or an action.
type State struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ExperimentAction is an action as it runs in an experiment.
type ExperimentAction struct {
	Action
	State     State  `json:"state"`
	StartTime *Epoch `json:"startTime,omitempty"`
	EndTime   *Epoch `json:"endTime,omitempty"`
}

// Experiment is a run of a template.
type Experiment struct {
	ID                   string                      `json:"id"`
	Arn                  string                      `json:"arn"`
	ExperimentTemplateID string                      `json:"experimentTemplateId"`
	RoleArn              string                      `json:"roleArn,omitempty"`
	State                State                       `json:"state"`
	Targets              map[string]Target           `json:"targets"`
	Actions              map[string]ExperimentAction `json:"actions"`
	StopConditions       []StopCondition             `json:"stopConditions"`
	CreationTime         Epoch                       `json:"creationTime"`
	StartTime            Epoch                       `json:"startTime"`
	EndTime              *Epoch                      `json:"endTime,omitempty"`
	Tags                 map[string]string           `json:"tags,omitempty"`
}

// ExperimentSummary is an experiment as ListExperiments shows it.
type ExperimentSummary struct {
	ID                   string            `json:"id"`
	Arn                  string            `json:"arn"`
	ExperimentTemplateID string            `json:"experimentTemplateId"`
	State                State             `json:"state"`
	CreationTime         Epoch             `json:"creationTime"`
	Tags                 map[string]string `json:"tags,omitempty"`
}

func (e Experiment) summary() ExperimentSummary {
	return ExperimentSummary{
		ID: e.ID, Arn: e.Arn, ExperimentTemplateID: e.ExperimentTemplateID,
		State: e.State, CreationTime: e.CreationTime, Tags: e.Tags,
	}
}
