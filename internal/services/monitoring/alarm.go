package monitoring

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stratus/internal/tags"
)

// Store buckets for alarms and their history.
const (
	alarmsBucket  = "cloudwatch-alarms"
	historyBucket = "cloudwatch-alarm-history"
)

type alarmState string

const (
	stateOK               alarmState = "OK"
	stateAlarm            alarmState = "ALARM"
	stateInsufficientData alarmState = "INSUFFICIENT_DATA"
)

func validState(s string) bool {
	switch alarmState(s) {
	case stateOK, stateAlarm, stateInsufficientData:
		return true
	}
	return false
}

type alarmKind string

const (
	kindMetric    alarmKind = "MetricAlarm"
	kindComposite alarmKind = "CompositeAlarm"
)

// alarm is a metric or composite alarm: its configuration and its current
// place in the INSUFFICIENT_DATA / OK / ALARM state machine.
type alarm struct {
	Name           string    `json:"name"`
	Kind           alarmKind `json:"kind"`
	ARN            string    `json:"arn"`
	Description    string    `json:"description,omitempty"`
	ActionsEnabled bool      `json:"actions_enabled"`
	OKActions      []string  `json:"ok_actions,omitempty"`
	AlarmActions   []string  `json:"alarm_actions,omitempty"`
	InsuffActions  []string  `json:"insufficient_data_actions,omitempty"`
	Tags           tags.List `json:"tags,omitempty"`

	// Metric alarms.
	Namespace           string          `json:"namespace,omitempty"`
	MetricName          string          `json:"metric_name,omitempty"`
	Dimensions          []dimension     `json:"dimensions,omitempty"`
	Statistic           string          `json:"statistic,omitempty"`
	ExtendedStatistic   string          `json:"extended_statistic,omitempty"`
	Period              int             `json:"period,omitempty"`
	Unit                string          `json:"unit,omitempty"`
	EvaluationPeriods   int             `json:"evaluation_periods,omitempty"`
	DatapointsToAlarm   int             `json:"datapoints_to_alarm,omitempty"`
	Threshold           *float64        `json:"threshold,omitempty"`
	ComparisonOperator  string          `json:"comparison_operator,omitempty"`
	TreatMissingData    string          `json:"treat_missing_data,omitempty"`
	EvaluateLowSamples  string          `json:"evaluate_low_sample_count_percentile,omitempty"`
	Metrics             json.RawMessage `json:"metrics,omitempty"`
	ThresholdMetricID   string          `json:"threshold_metric_id,omitempty"`
	LastEvaluatedBucket time.Time       `json:"last_evaluated_bucket,omitempty"`

	// Composite alarms.
	Rule                string `json:"rule,omitempty"`
	Suppressor          string `json:"actions_suppressor,omitempty"`
	SuppressorWait      int    `json:"actions_suppressor_wait_period,omitempty"`
	SuppressorExtension int    `json:"actions_suppressor_extension_period,omitempty"`

	State              alarmState `json:"state"`
	StateReason        string     `json:"state_reason"`
	StateReasonData    string     `json:"state_reason_data,omitempty"`
	StateUpdated       time.Time  `json:"state_updated"`
	StateTransitioned  time.Time  `json:"state_transitioned"`
	ConfigUpdated      time.Time  `json:"config_updated"`
	Created            int64      `json:"created"` // creation order, for DescribeAlarmsForMetric
	pendingTransitions []transition
}

// transition is a state change an alarm went through, awaiting its history
// item and actions.
type transition struct {
	From, To       alarmState
	FromReason     string
	FromReasonData string
	Reason         string
	ReasonData     string
	At             time.Time
}

// setState moves the alarm to state and reports whether that was a change. A
// change is queued for history and actions; the same state only refreshes
// the reason, as CloudWatch does.
func (a *alarm) setState(state alarmState, reason, reasonData string, now time.Time) bool {
	if a.State == state {
		a.StateReason, a.StateReasonData, a.StateUpdated = reason, reasonData, now
		return false
	}
	a.pendingTransitions = append(a.pendingTransitions, transition{
		From: a.State, To: state, FromReason: a.StateReason, FromReasonData: a.StateReasonData, Reason: reason, ReasonData: reasonData, At: now,
	})
	a.State, a.StateReason, a.StateReasonData = state, reason, reasonData
	a.StateUpdated, a.StateTransitioned = now, now
	return true
}

// actionsFor returns the actions configured for entering state.
func (a *alarm) actionsFor(state alarmState) []string {
	switch state {
	case stateOK:
		return a.OKActions
	case stateAlarm:
		return a.AlarmActions
	default:
		return a.InsuffActions
	}
}

// historyItem is one DescribeAlarmHistory entry.
type historyItem struct {
	AlarmName string    `json:"alarm_name"`
	AlarmType alarmKind `json:"alarm_type"`
	Timestamp time.Time `json:"timestamp"`
	ItemType  string    `json:"item_type"` // ConfigurationUpdate | StateUpdate | Action
	Summary   string    `json:"summary"`
	Data      string    `json:"data"`
}

// stateHistoryData is the HistoryData JSON CloudWatch records for a state
// change.
func stateHistoryData(t transition) string {
	type state struct {
		StateValue      alarmState      `json:"stateValue"`
		StateReason     string          `json:"stateReason"`
		StateReasonData json.RawMessage `json:"stateReasonData,omitempty"`
	}
	withData := func(v alarmState, reason, data string) state {
		st := state{StateValue: v, StateReason: reason}
		if data != "" && json.Valid([]byte(data)) {
			st.StateReasonData = json.RawMessage(data)
		}
		return st
	}
	raw, _ := json.Marshal(struct {
		Version  string `json:"version"`
		OldState state  `json:"oldState"`
		NewState state  `json:"newState"`
	}{"1.0", withData(t.From, t.FromReason, t.FromReasonData), withData(t.To, t.Reason, t.ReasonData)})
	return string(raw)
}

func stateHistorySummary(t transition) string {
	return fmt.Sprintf("Alarm updated from %s to %s", t.From, t.To)
}
