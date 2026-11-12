package monitoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file runs alarms: it persists them, evaluates metric alarms against
// the stored datapoints, derives composite alarms from their children, and
// records each state change in history and in the alarm's actions.
//
// There is no background evaluator. CloudWatch evaluates each alarm once per
// period, over the periods that have completed; Stratus does the same lazily,
// on the first CloudWatch call in each new period (a write of data or any
// alarm read), so results match without a ticker.

func (s *Service) loadAlarm(name string) (*alarm, error) {
	raw, err := s.metadata.Get(alarmsBucket, name)
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return nil, nil
	}
	var a alarm
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, internal(err)
	}
	return &a, nil
}

func (s *Service) storeAlarm(a alarm) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(alarmsBucket, a.Name, raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) allAlarms() ([]alarm, error) {
	var out []alarm
	err := s.metadata.Scan(alarmsBucket, "", func(_, v []byte) error {
		var a alarm
		if err := json.Unmarshal(v, &a); err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

func (s *Service) appendHistory(h historyItem) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return internal(err)
	}
	key := h.AlarmName + "|" + h.Timestamp.Format(time.RFC3339Nano) + "|" + uuid.NewString()
	if err := s.metadata.Put(historyBucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

// metricPoints returns the stored datapoints of one metric.
func (s *Service) metricPoints(namespace, name string) ([]metricDatum, error) {
	var out []metricDatum
	err := s.metadata.Scan(metricsBucket, namespace+"|"+name+"|", func(_, v []byte) error {
		var d metricDatum
		if json.Unmarshal(v, &d) == nil && d.Namespace == namespace && d.MetricName == name {
			out = append(out, d)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// evaluable reports whether the evaluator can run a, a single-metric alarm.
// Metric-math alarms are stored and described but not evaluated.
func evaluable(a alarm) bool {
	return a.Kind == kindMetric && a.Period > 0 && a.Threshold != nil && len(a.Metrics) == 0
}

// evaluateAlarms evaluates the metric alarms selected by match, then
// derives composites and records the resulting changes.
func (s *Service) evaluateAlarms(match func(alarm) bool) error {
	all, err := s.allAlarms()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	var touched []*alarm
	for i := range all {
		a := &all[i]
		if !evaluable(*a) || !match(*a) {
			continue
		}
		points, err := s.metricPoints(a.Namespace, a.MetricName)
		if err != nil {
			return err
		}
		if v := evaluate(*a, points, now); !v.keep && v.state != a.State {
			a.setState(v.state, v.reason, v.reasonData, now)
		}
		a.LastEvaluatedBucket = periodStart(now, a.Period)
		touched = append(touched, a)
	}
	return s.commit(touched)
}

// evaluateDue runs the metric alarms whose evaluator has not yet run in the
// current period.
func (s *Service) evaluateDue() error {
	now := s.now().UTC()
	return s.evaluateAlarms(func(a alarm) bool {
		return periodStart(now, a.Period).After(a.LastEvaluatedBucket)
	})
}

// commit stores changed alarms, re-derives every composite alarm until the
// states settle, then writes history and runs actions for each transition.
func (s *Service) commit(changed []*alarm) error {
	for _, a := range changed {
		if err := s.storeAlarm(*a); err != nil {
			return err
		}
	}
	all, err := s.allAlarms()
	if err != nil {
		return err
	}
	byName := map[string]*alarm{}
	for i := range all {
		byName[all[i].Name] = &all[i]
	}
	for _, a := range changed {
		byName[a.Name].pendingTransitions = a.pendingTransitions
	}

	now := s.now().UTC()
	// Composites may reference composites; a few rounds settle any chain.
	for round := 0; round < 10; round++ {
		moved := false
		for _, a := range all {
			c := byName[a.Name]
			if c.Kind != kindComposite {
				continue
			}
			rule, err := parseAlarmRule(c.Rule)
			if err != nil {
				continue
			}
			stateOf := func(child string) alarmState {
				if ch, ok := byName[alarmNameOf(child)]; ok {
					return ch.State
				}
				return stateInsufficientData
			}
			want := stateOK
			if rule.holds(stateOf) {
				want = stateAlarm
			}
			if want == c.State {
				continue
			}
			reason, data := compositeReason(ruleChildren(rule), byName, now)
			c.setState(want, reason, data, now)
			moved = true
		}
		if !moved {
			break
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := byName[name]
		if len(a.pendingTransitions) == 0 {
			continue
		}
		if err := s.storeAlarm(*a); err != nil {
			return err
		}
		for _, t := range a.pendingTransitions {
			if err := s.appendHistory(historyItem{AlarmName: a.Name, AlarmType: a.Kind, Timestamp: t.At,
				ItemType: "StateUpdate", Summary: stateHistorySummary(t), Data: stateHistoryData(t)}); err != nil {
				return err
			}
			if err := s.runActions(*a, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// compositeReason names the child whose latest transition decided the
// composite's state, as CloudWatch's composite StateReason and
// StateReasonData (triggeringAlarms) do.
func compositeReason(children []string, byName map[string]*alarm, now time.Time) (string, string) {
	var trigger *alarm
	for _, name := range children {
		ch, ok := byName[name]
		if !ok {
			continue
		}
		if trigger == nil || ch.StateTransitioned.After(trigger.StateTransitioned) {
			trigger = ch
		}
	}
	if trigger == nil {
		return "Composite alarm evaluated at " + now.Format(time.RFC1123), ""
	}
	at := trigger.StateTransitioned.UTC()
	data, _ := json.Marshal(map[string]any{"triggeringAlarms": []any{map[string]any{
		"arn": trigger.ARN, "state": map[string]any{"value": trigger.State, "timestamp": at.Format(isoMillis)},
	}}})
	return fmt.Sprintf("%s transitioned to %s at %s", trigger.ARN, trigger.State, at.Format("Monday 02 January, 2006 15:04:05 UTC")), string(data)
}

// runActions publishes the alarm's notification to the SNS topics configured
// for the state it entered, recording each attempt in history.
func (s *Service) runActions(a alarm, t transition) error {
	if !a.ActionsEnabled {
		return nil
	}
	for _, action := range a.actionsFor(t.To) {
		if !strings.Contains(action, ":sns:") || s.sns == nil {
			continue
		}
		message := s.notification(a, t)
		summary, actionState := "Successfully executed action "+action, "Succeeded"
		if err := s.sns.PublishToTopic(action, message); err != nil {
			summary, actionState = "Failed to execute action "+action, "Failed"
		}
		data, _ := json.Marshal(map[string]any{
			"actionState": actionState, "stateUpdateTimestamp": t.At.UnixMilli(),
			"notificationResource": action, "publishedMessage": message,
		})
		if err := s.appendHistory(historyItem{AlarmName: a.Name, AlarmType: a.Kind, Timestamp: s.now().UTC(),
			ItemType: "Action", Summary: summary, Data: string(data)}); err != nil {
			return err
		}
	}
	return nil
}

// notification is the JSON message CloudWatch publishes to SNS.
func (s *Service) notification(a alarm, t transition) string {
	msg := map[string]any{
		"AlarmName":                          a.Name,
		"AlarmDescription":                   nullable(a.Description),
		"AWSAccountId":                       s.scope.Account,
		"AlarmConfigurationUpdatedTimestamp": stamp(a.ConfigUpdated),
		"NewStateValue":                      t.To,
		"NewStateReason":                     t.Reason,
		"StateChangeTime":                    stamp(t.At),
		"Region":                             s.scope.Region,
		"AlarmArn":                           a.ARN,
		"OldStateValue":                      t.From,
		"OKActions":                          orEmpty(a.OKActions),
		"AlarmActions":                       orEmpty(a.AlarmActions),
		"InsufficientDataActions":            orEmpty(a.InsuffActions),
	}
	if a.Kind == kindComposite {
		msg["AlarmRule"] = a.Rule
	} else {
		dims := []map[string]string{}
		for _, d := range a.Dimensions {
			dims = append(dims, map[string]string{"name": d.Name, "value": d.Value})
		}
		msg["Trigger"] = map[string]any{
			"MetricName": a.MetricName, "Namespace": a.Namespace, "StatisticType": "Statistic",
			"Statistic": strings.ToUpper(a.Statistic), "Unit": nullable(a.Unit), "Dimensions": dims,
			"Period": a.Period, "EvaluationPeriods": a.EvaluationPeriods, "ComparisonOperator": a.ComparisonOperator,
			"Threshold": a.Threshold, "TreatMissingData": a.TreatMissingData,
		}
	}
	raw, _ := json.Marshal(msg)
	return string(raw)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
