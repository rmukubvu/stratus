package monitoring

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/tags"
)

// TopicPublisher delivers alarm notifications to SNS topics.
type TopicPublisher interface {
	PublishToTopic(topicARN, message string) error
}

// SetScope sets the account and region alarm ARNs are minted in.
func (s *Service) SetScope(sc scope.Scope) { s.scope = sc }

// SetSNS lets alarm actions publish to SNS topics.
func (s *Service) SetSNS(p TopicPublisher) { s.sns = p }

// alarmOperations are the alarm actions, by name.
var alarmOperations = map[string]func(*Service, http.ResponseWriter, *http.Request, string) error{
	"PutMetricAlarm":          (*Service).putMetricAlarm,
	"PutCompositeAlarm":       (*Service).putCompositeAlarm,
	"DescribeAlarms":          (*Service).describeAlarms,
	"DescribeAlarmsForMetric": (*Service).describeAlarmsForMetric,
	"DeleteAlarms":            (*Service).deleteAlarms,
	"SetAlarmState":           (*Service).setAlarmState,
	"DescribeAlarmHistory":    (*Service).describeAlarmHistory,
	"EnableAlarmActions": func(s *Service, w http.ResponseWriter, r *http.Request, id string) error {
		return s.toggleActions(w, r, id, true)
	},
	"DisableAlarmActions": func(s *Service, w http.ResponseWriter, r *http.Request, id string) error {
		return s.toggleActions(w, r, id, false)
	},
	"TagResource":         (*Service).tagResource,
	"UntagResource":       (*Service).untagResource,
	"ListTagsForResource": (*Service).listTagsForResource,
}

// AlarmOperations lists the alarm actions, for request classification.
func AlarmOperations() []string {
	out := make([]string, 0, len(alarmOperations))
	for name := range alarmOperations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func validationError(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: message}
}

func resourceNotFound(code string) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: code}
}

// ---- request and response shapes (AWS JSON field names) ----

type dimensionIO struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type tagIO struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

func tagList(in []tagIO) tags.List {
	out := make(tags.List, 0, len(in))
	for _, t := range in {
		out = append(out, tags.Tag{Key: t.Key, Value: t.Value})
	}
	return out
}

type commonAlarmInput struct {
	AlarmName               string    `json:"AlarmName"`
	AlarmDescription        string    `json:"AlarmDescription"`
	ActionsEnabled          *flexBool `json:"ActionsEnabled"`
	OKActions               []string  `json:"OKActions"`
	AlarmActions            []string  `json:"AlarmActions"`
	InsufficientDataActions []string  `json:"InsufficientDataActions"`
	Tags                    []tagIO   `json:"Tags"`
}

type putMetricAlarmInput struct {
	commonAlarmInput
	MetricName                       string          `json:"MetricName"`
	Namespace                        string          `json:"Namespace"`
	Statistic                        string          `json:"Statistic"`
	ExtendedStatistic                string          `json:"ExtendedStatistic"`
	Dimensions                       []dimensionIO   `json:"Dimensions"`
	Period                           *flexInt        `json:"Period"`
	Unit                             string          `json:"Unit"`
	EvaluationPeriods                *flexInt        `json:"EvaluationPeriods"`
	DatapointsToAlarm                *flexInt        `json:"DatapointsToAlarm"`
	Threshold                        *flexFloat      `json:"Threshold"`
	ComparisonOperator               string          `json:"ComparisonOperator"`
	TreatMissingData                 string          `json:"TreatMissingData"`
	EvaluateLowSampleCountPercentile string          `json:"EvaluateLowSampleCountPercentile"`
	Metrics                          json.RawMessage `json:"Metrics"`
	ThresholdMetricID                string          `json:"ThresholdMetricId"`
}

type putCompositeAlarmInput struct {
	commonAlarmInput
	AlarmRule                        string   `json:"AlarmRule"`
	ActionsSuppressor                string   `json:"ActionsSuppressor"`
	ActionsSuppressorWaitPeriod      *flexInt `json:"ActionsSuppressorWaitPeriod"`
	ActionsSuppressorExtensionPeriod *flexInt `json:"ActionsSuppressorExtensionPeriod"`
}

type alarmOut struct {
	AlarmName                          string          `json:"AlarmName"`
	AlarmArn                           string          `json:"AlarmArn"`
	AlarmDescription                   string          `json:"AlarmDescription,omitempty"`
	AlarmConfigurationUpdatedTimestamp string          `json:"AlarmConfigurationUpdatedTimestamp"`
	ActionsEnabled                     bool            `json:"ActionsEnabled"`
	OKActions                          []string        `json:"OKActions"`
	AlarmActions                       []string        `json:"AlarmActions"`
	InsufficientDataActions            []string        `json:"InsufficientDataActions"`
	StateValue                         alarmState      `json:"StateValue"`
	StateReason                        string          `json:"StateReason"`
	StateReasonData                    string          `json:"StateReasonData,omitempty"`
	StateUpdatedTimestamp              string          `json:"StateUpdatedTimestamp"`
	StateTransitionedTimestamp         string          `json:"StateTransitionedTimestamp"`
	MetricName                         string          `json:"MetricName,omitempty"`
	Namespace                          string          `json:"Namespace,omitempty"`
	Statistic                          string          `json:"Statistic,omitempty"`
	ExtendedStatistic                  string          `json:"ExtendedStatistic,omitempty"`
	Dimensions                         []dimensionIO   `json:"Dimensions,omitempty"`
	Period                             int             `json:"Period,omitempty"`
	Unit                               string          `json:"Unit,omitempty"`
	EvaluationPeriods                  int             `json:"EvaluationPeriods,omitempty"`
	DatapointsToAlarm                  int             `json:"DatapointsToAlarm,omitempty"`
	Threshold                          *jsonDouble     `json:"Threshold,omitempty"`
	ComparisonOperator                 string          `json:"ComparisonOperator,omitempty"`
	TreatMissingData                   string          `json:"TreatMissingData,omitempty"`
	EvaluateLowSampleCountPercentile   string          `json:"EvaluateLowSampleCountPercentile,omitempty"`
	Metrics                            json.RawMessage `json:"Metrics,omitempty"`
	ThresholdMetricID                  string          `json:"ThresholdMetricId,omitempty"`
	AlarmRule                          string          `json:"AlarmRule,omitempty"`
	ActionsSuppressor                  string          `json:"ActionsSuppressor,omitempty"`
	ActionsSuppressorWaitPeriod        int             `json:"ActionsSuppressorWaitPeriod,omitempty"`
	ActionsSuppressorExtensionPeriod   int             `json:"ActionsSuppressorExtensionPeriod,omitempty"`
}

// jsonDouble marshals as CloudWatch writes doubles (90.0, not 90).
type jsonDouble float64

func (d jsonDouble) MarshalJSON() ([]byte, error) { return []byte(javaDouble(float64(d))), nil }

const timestampFormat = "2006-01-02T15:04:05.000Z"

func stamp(t time.Time) string { return t.UTC().Format(timestampFormat) }

func toOut(a alarm) alarmOut {
	out := alarmOut{
		AlarmName: a.Name, AlarmArn: a.ARN, AlarmDescription: a.Description,
		AlarmConfigurationUpdatedTimestamp: stamp(a.ConfigUpdated), ActionsEnabled: a.ActionsEnabled,
		OKActions: orEmpty(a.OKActions), AlarmActions: orEmpty(a.AlarmActions), InsufficientDataActions: orEmpty(a.InsuffActions),
		StateValue: a.State, StateReason: a.StateReason, StateReasonData: a.StateReasonData,
		StateUpdatedTimestamp: stamp(a.StateUpdated), StateTransitionedTimestamp: stamp(a.StateTransitioned),
		AlarmRule: a.Rule, ActionsSuppressor: a.Suppressor,
		ActionsSuppressorWaitPeriod: a.SuppressorWait, ActionsSuppressorExtensionPeriod: a.SuppressorExtension,
	}
	if a.Kind == kindMetric {
		out.MetricName, out.Namespace, out.Statistic, out.ExtendedStatistic = a.MetricName, a.Namespace, a.Statistic, a.ExtendedStatistic
		out.Period, out.Unit, out.EvaluationPeriods, out.DatapointsToAlarm = a.Period, a.Unit, a.EvaluationPeriods, a.DatapointsToAlarm
		out.Threshold, out.ComparisonOperator, out.TreatMissingData = (*jsonDouble)(a.Threshold), a.ComparisonOperator, a.TreatMissingData
		out.EvaluateLowSampleCountPercentile, out.Metrics, out.ThresholdMetricID = a.EvaluateLowSamples, a.Metrics, a.ThresholdMetricID
		out.Dimensions = []dimensionIO{}
		for _, d := range a.Dimensions {
			out.Dimensions = append(out.Dimensions, dimensionIO{d.Name, d.Value})
		}
	}
	return out
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---- PutMetricAlarm / PutCompositeAlarm ----

var comparisonOperators = map[string]bool{
	"GreaterThanThreshold": true, "GreaterThanOrEqualToThreshold": true,
	"LessThanThreshold": true, "LessThanOrEqualToThreshold": true,
	"LessThanLowerOrGreaterThanUpperThreshold": true, "LessThanLowerThreshold": true, "GreaterThanUpperThreshold": true,
}

var statistics = map[string]bool{"Average": true, "Sum": true, "Minimum": true, "Maximum": true, "SampleCount": true}

// newMetricAlarm validates a PutMetricAlarm request into the alarm it
// configures, the way CloudWatch does.
func newMetricAlarm(in putMetricAlarmInput) (alarm, error) {
	a := alarm{Kind: kindMetric}
	if err := applyCommon(&a, in.commonAlarmInput); err != nil {
		return a, err
	}
	if in.EvaluationPeriods == nil {
		return a, missing("EvaluationPeriods")
	}
	if in.ComparisonOperator == "" {
		return a, missing("ComparisonOperator")
	}
	if !comparisonOperators[in.ComparisonOperator] {
		return a, invalidValue(in.ComparisonOperator, "ComparisonOperator")
	}
	a.EvaluationPeriods, a.ComparisonOperator = int(*in.EvaluationPeriods), in.ComparisonOperator
	if a.EvaluationPeriods < 1 {
		return a, invalidValue(strconv.Itoa(a.EvaluationPeriods), "EvaluationPeriods")
	}
	if in.DatapointsToAlarm != nil {
		a.DatapointsToAlarm = int(*in.DatapointsToAlarm)
		if a.DatapointsToAlarm < 1 {
			return a, invalidValue(strconv.Itoa(a.DatapointsToAlarm), "DatapointsToAlarm")
		}
		if a.DatapointsToAlarm > a.EvaluationPeriods {
			return a, validationError("DatapointsToAlarm must be less than or equal to EvaluationPeriods")
		}
	}
	switch in.TreatMissingData {
	case "", "breaching", "notBreaching", "ignore", "missing":
		a.TreatMissingData = in.TreatMissingData
	default:
		return a, validationError(fmt.Sprintf("The value %s is not supported for TreatMissingData parameter. "+
			"Supported values are [breaching, notBreaching, ignore, missing].", in.TreatMissingData))
	}
	switch in.EvaluateLowSampleCountPercentile {
	case "", "evaluate", "ignore":
		a.EvaluateLowSamples = in.EvaluateLowSampleCountPercentile
	default:
		return a, invalidValue(in.EvaluateLowSampleCountPercentile, "EvaluateLowSampleCountPercentile")
	}
	if in.Threshold != nil {
		t := float64(*in.Threshold)
		a.Threshold = &t
	}

	if len(in.Metrics) > 0 && string(in.Metrics) != "null" {
		if in.MetricName != "" || in.Namespace != "" || in.Statistic != "" || in.ExtendedStatistic != "" || in.Period != nil {
			return a, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterCombination",
				Message: "The parameters MetricName, Namespace, Statistic, ExtendedStatistic, Dimensions, Period and Unit cannot be specified with Metrics."}
		}
		var members []struct {
			ReturnData *flexBool `json:"ReturnData"`
		}
		if err := json.Unmarshal(in.Metrics, &members); err != nil || len(members) == 0 {
			return a, invalidValue(string(in.Metrics), "Metrics")
		}
		returning := 0
		for _, m := range members {
			if m.ReturnData == nil || bool(*m.ReturnData) {
				returning++
			}
		}
		if returning != 1 && in.ThresholdMetricID == "" {
			return a, validationError("Exactly one element of the metrics list should return data.")
		}
		a.Metrics, a.ThresholdMetricID = in.Metrics, in.ThresholdMetricID
		if a.Threshold == nil && a.ThresholdMetricID == "" {
			return a, missing("Threshold")
		}
		return a, nil
	}

	if in.MetricName == "" {
		return a, missing("MetricName")
	}
	if in.Namespace == "" {
		return a, missing("Namespace")
	}
	if strings.HasPrefix(in.Namespace, "AWS/") && in.Namespace == "AWS/" {
		return a, invalidValue(in.Namespace, "Namespace")
	}
	switch {
	case in.Statistic != "" && in.ExtendedStatistic != "":
		return a, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterCombination",
			Message: "Statistic and ExtendedStatistic cannot both be specified."}
	case in.Statistic == "" && in.ExtendedStatistic == "":
		return a, missing("Statistic")
	case in.Statistic != "" && !statistics[in.Statistic]:
		return a, invalidValue(in.Statistic, "Statistic")
	case in.ExtendedStatistic != "":
		if _, ok := percentile(in.ExtendedStatistic); !ok {
			return a, invalidValue(in.ExtendedStatistic, "ExtendedStatistic")
		}
	}
	if in.Period == nil {
		return a, missing("Period")
	}
	period := int(*in.Period)
	if period <= 0 || (period%60 != 0 && period != 10 && period != 20 && period != 30) {
		return a, validationError("Period must be 10, 20, 30 or a multiple of 60")
	}
	if err := checkHorizon(period, a.EvaluationPeriods); err != nil {
		return a, err
	}
	if a.Threshold == nil {
		return a, missing("Threshold")
	}
	a.MetricName, a.Namespace, a.Statistic, a.ExtendedStatistic = in.MetricName, in.Namespace, in.Statistic, in.ExtendedStatistic
	a.Period, a.Unit = period, in.Unit
	for _, d := range in.Dimensions {
		a.Dimensions = append(a.Dimensions, dimension{Name: d.Name, Value: d.Value})
	}
	return a, nil
}

// checkHorizon enforces CloudWatch's limit on how far back an alarm looks:
// a day for periods under an hour, a week otherwise.
func checkHorizon(period, evaluationPeriods int) error {
	span := period * evaluationPeriods
	if period < 3600 && span > 86400 {
		return validationError("Metrics cannot be checked across more than a day (EvaluationPeriods * Period must be <= 86400) for alarms using period < 3600")
	}
	if period >= 3600 && span > 604800 {
		return validationError("Metrics cannot be checked across more than a week (EvaluationPeriods * Period must be <= 604800) for alarms using period >= 3600")
	}
	return nil
}

func applyCommon(a *alarm, in commonAlarmInput) error {
	if in.AlarmName == "" {
		return missing("AlarmName")
	}
	if len(in.AlarmName) > 255 {
		return invalidValue(in.AlarmName, "AlarmName")
	}
	a.Name, a.Description = in.AlarmName, in.AlarmDescription
	a.ActionsEnabled = in.ActionsEnabled == nil || bool(*in.ActionsEnabled)
	a.OKActions, a.AlarmActions, a.InsuffActions = in.OKActions, in.AlarmActions, in.InsufficientDataActions
	a.Tags = tagList(in.Tags)
	if err := a.Tags.Validate(tags.MaxPerResource); err != nil {
		return validationError(err.Error())
	}
	return nil
}

func missing(param string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "MissingParameter",
		Message: fmt.Sprintf("The parameter %s is required.", param)}
}

func invalidValue(value, param string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterValue",
		Message: fmt.Sprintf("The value %s for parameter %s is invalid.", value, param)}
}

func (s *Service) putMetricAlarm(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in putMetricAlarmInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	a, err := newMetricAlarm(in)
	if err != nil {
		return err
	}
	if err := s.saveConfiguration(a); err != nil {
		return err
	}
	return respond(w, r, "PutMetricAlarm", requestID, nil)
}

func (s *Service) putCompositeAlarm(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in putCompositeAlarmInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	a := alarm{Kind: kindComposite}
	if err := applyCommon(&a, in.commonAlarmInput); err != nil {
		return err
	}
	if in.AlarmRule == "" {
		return missing("AlarmRule")
	}
	rule, err := parseAlarmRule(in.AlarmRule)
	if err != nil {
		return validationError("The AlarmRule is invalid: " + err.Error())
	}
	var absent []string
	for _, child := range ruleChildren(rule) {
		if child == a.Name {
			return validationError("A composite alarm cannot reference itself in its AlarmRule.")
		}
		if existing, err := s.loadAlarm(child); err != nil {
			return err
		} else if existing == nil {
			absent = append(absent, child)
		}
	}
	if len(absent) > 0 {
		return validationError(fmt.Sprintf("Could not save the composite alarm as alarms [%s] in the alarm rule do not exist", strings.Join(absent, ", ")))
	}
	if in.ActionsSuppressor != "" {
		suppressor, err := s.loadAlarm(alarmNameOf(in.ActionsSuppressor))
		if err != nil {
			return err
		}
		if suppressor == nil {
			return validationError(fmt.Sprintf("Could not save the composite alarm as the actions suppressor alarm [%s] does not exist",
				s.scope.ARN("cloudwatch", "alarm:"+alarmNameOf(in.ActionsSuppressor))))
		}
		a.Suppressor = in.ActionsSuppressor
		if in.ActionsSuppressorWaitPeriod != nil {
			a.SuppressorWait = int(*in.ActionsSuppressorWaitPeriod)
		}
		if in.ActionsSuppressorExtensionPeriod != nil {
			a.SuppressorExtension = int(*in.ActionsSuppressorExtensionPeriod)
		}
	}
	a.Rule = in.AlarmRule
	if err := s.saveConfiguration(a); err != nil {
		return err
	}
	if err := s.commit(nil); err != nil {
		return err
	}
	return respond(w, r, "PutCompositeAlarm", requestID, nil)
}

// saveConfiguration creates or updates an alarm's configuration, keeping its
// state, history and creation order across updates.
func (s *Service) saveConfiguration(a alarm) error {
	now := s.now().UTC()
	existing, err := s.loadAlarm(a.Name)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("Alarm %q created", a.Name)
	if existing != nil {
		if existing.Kind != a.Kind {
			return validationError(fmt.Sprintf("AlarmName [%s] is already used by another alarm of a different type", a.Name))
		}
		a.State, a.StateReason, a.StateReasonData = existing.State, existing.StateReason, existing.StateReasonData
		a.StateUpdated, a.StateTransitioned, a.Created = existing.StateUpdated, existing.StateTransitioned, existing.Created
		if len(a.Tags) == 0 {
			a.Tags = existing.Tags
		}
		summary = fmt.Sprintf("Alarm %q updated", a.Name)
	} else {
		a.State, a.StateReason = stateInsufficientData, "Unchecked: Initial alarm creation"
		a.StateUpdated, a.StateTransitioned, a.Created = now, now, now.UnixNano()
	}
	a.ARN = s.scope.ARN("cloudwatch", "alarm:"+a.Name)
	a.ConfigUpdated = now
	if a.Period > 0 {
		a.LastEvaluatedBucket = periodStart(now, a.Period) // first evaluated when this period completes
	}
	if err := s.storeAlarm(a); err != nil {
		return err
	}
	return s.appendHistory(historyItem{AlarmName: a.Name, AlarmType: a.Kind, Timestamp: now, ItemType: "ConfigurationUpdate", Summary: summary,
		Data: configurationData(a, existing == nil)})
}

func configurationData(a alarm, created bool) string {
	kind := "Update"
	if created {
		kind = "Create"
	}
	raw, _ := json.Marshal(map[string]any{"version": "1.0", "type": kind, "updatedAlarm": toOut(a)})
	return string(raw)
}

// ---- DescribeAlarms / DescribeAlarmsForMetric ----

type describeAlarmsInput struct {
	AlarmNames          []string `json:"AlarmNames"`
	AlarmNamePrefix     string   `json:"AlarmNamePrefix"`
	AlarmTypes          []string `json:"AlarmTypes"`
	ChildrenOfAlarmName string   `json:"ChildrenOfAlarmName"`
	ParentsOfAlarmName  string   `json:"ParentsOfAlarmName"`
	StateValue          string   `json:"StateValue"`
	ActionPrefix        string   `json:"ActionPrefix"`
	MaxRecords          *flexInt `json:"MaxRecords"`
	NextToken           string   `json:"NextToken"`
}

type describeAlarmsOutput struct {
	MetricAlarms    []alarmOut `json:"MetricAlarms"`
	CompositeAlarms []alarmOut `json:"CompositeAlarms"`
	NextToken       string     `json:"NextToken,omitempty"`
}

func (s *Service) describeAlarms(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in describeAlarmsInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	if in.StateValue != "" && !validState(in.StateValue) {
		return invalidValue(in.StateValue, "StateValue")
	}
	if (in.ChildrenOfAlarmName != "" || in.ParentsOfAlarmName != "") && (len(in.AlarmNames) > 0 || in.AlarmNamePrefix != "") {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterCombination",
			Message: "ChildrenOfAlarmName and ParentsOfAlarmName cannot be used with AlarmNames or AlarmNamePrefix."}
	}
	if err := s.evaluateDue(); err != nil {
		return err
	}
	all, err := s.allAlarms()
	if err != nil {
		return err
	}
	byName := map[string]alarm{}
	for _, a := range all {
		byName[a.Name] = a
	}
	related := map[string]bool{}
	if in.ChildrenOfAlarmName != "" {
		if parent, ok := byName[in.ChildrenOfAlarmName]; ok && parent.Kind == kindComposite {
			if rule, err := parseAlarmRule(parent.Rule); err == nil {
				for _, c := range ruleChildren(rule) {
					related[c] = true
				}
			}
		}
	}
	if in.ParentsOfAlarmName != "" {
		for _, a := range all {
			if a.Kind == kindComposite && references(a, in.ParentsOfAlarmName) {
				related[a.Name] = true
			}
		}
	}
	types := map[alarmKind]bool{kindMetric: len(in.AlarmTypes) == 0}
	for _, t := range in.AlarmTypes {
		if t != string(kindMetric) && t != string(kindComposite) {
			return invalidValue(t, "AlarmTypes")
		}
		types[alarmKind(t)] = true
	}
	if in.ChildrenOfAlarmName != "" || in.ParentsOfAlarmName != "" {
		types[kindMetric] = types[kindMetric] || len(in.AlarmTypes) == 0
		if len(in.AlarmTypes) == 0 {
			types[kindComposite] = true
		}
	}

	var matched []alarm
	for _, a := range all {
		switch {
		case !types[a.Kind]:
		case len(in.AlarmNames) > 0 && !containsString(in.AlarmNames, a.Name):
		case in.AlarmNamePrefix != "" && !strings.HasPrefix(a.Name, in.AlarmNamePrefix):
		case (in.ChildrenOfAlarmName != "" || in.ParentsOfAlarmName != "") && !related[a.Name]:
		case in.StateValue != "" && string(a.State) != in.StateValue:
		case in.ActionPrefix != "" && !anyWithPrefix(a, in.ActionPrefix):
		default:
			matched = append(matched, a)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Name < matched[j].Name })
	page, next, err := paginate(matched, in.MaxRecords, in.NextToken, 100)
	if err != nil {
		return err
	}
	out := describeAlarmsOutput{MetricAlarms: []alarmOut{}, CompositeAlarms: []alarmOut{}, NextToken: next}
	for _, a := range page {
		if a.Kind == kindMetric {
			out.MetricAlarms = append(out.MetricAlarms, toOut(a))
		} else {
			out.CompositeAlarms = append(out.CompositeAlarms, toOut(a))
		}
	}
	return respond(w, r, "DescribeAlarms", requestID, out)
}

func references(composite alarm, child string) bool {
	rule, err := parseAlarmRule(composite.Rule)
	return err == nil && containsString(ruleChildren(rule), alarmNameOf(child))
}

func anyWithPrefix(a alarm, prefix string) bool {
	for _, list := range [][]string{a.OKActions, a.AlarmActions, a.InsuffActions} {
		for _, action := range list {
			if strings.HasPrefix(action, prefix) {
				return true
			}
		}
	}
	return false
}

func paginate[T any](items []T, maxRecords *flexInt, token string, limit int) ([]T, string, error) {
	size := limit
	if maxRecords != nil {
		size = int(*maxRecords)
		if size < 1 || size > limit {
			return nil, "", invalidValue(strconv.Itoa(size), "MaxRecords")
		}
	}
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidNextToken", Message: "Invalid NextToken : " + token}
		}
		start = n
	}
	end := start + size
	if end >= len(items) {
		return items[start:], "", nil
	}
	return items[start:end], strconv.Itoa(end), nil
}

type describeAlarmsForMetricInput struct {
	MetricName        string        `json:"MetricName"`
	Namespace         string        `json:"Namespace"`
	Statistic         string        `json:"Statistic"`
	ExtendedStatistic string        `json:"ExtendedStatistic"`
	Dimensions        []dimensionIO `json:"Dimensions"`
	Period            *flexInt      `json:"Period"`
	Unit              string        `json:"Unit"`
}

func (s *Service) describeAlarmsForMetric(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in describeAlarmsForMetricInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	if in.MetricName == "" {
		return missing("MetricName")
	}
	if in.Namespace == "" {
		return missing("Namespace")
	}
	if err := s.evaluateDue(); err != nil {
		return err
	}
	all, err := s.allAlarms()
	if err != nil {
		return err
	}
	var want []dimension
	for _, d := range in.Dimensions {
		want = append(want, dimension{Name: d.Name, Value: d.Value})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Created < all[j].Created })
	out := []alarmOut{}
	for _, a := range all {
		switch {
		case a.Kind != kindMetric, a.MetricName != in.MetricName, a.Namespace != in.Namespace:
		case in.Statistic != "" && a.Statistic != in.Statistic:
		case in.ExtendedStatistic != "" && a.ExtendedStatistic != in.ExtendedStatistic:
		case in.Period != nil && a.Period != int(*in.Period):
		case in.Unit != "" && a.Unit != in.Unit:
		case len(want) > 0 && !sameDimensions(sortedDimensions(a.Dimensions), sortedDimensions(want)):
		default:
			out = append(out, toOut(a))
		}
	}
	return respond(w, r, "DescribeAlarmsForMetric", requestID, struct {
		MetricAlarms []alarmOut `json:"MetricAlarms"`
	}{out})
}

// ---- DeleteAlarms / SetAlarmState / Enable+DisableAlarmActions ----

type alarmNamesInput struct {
	AlarmNames []string `json:"AlarmNames"`
}

func (s *Service) deleteAlarms(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in alarmNamesInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	all, err := s.allAlarms()
	if err != nil {
		return err
	}
	byName := map[string]alarm{}
	for _, a := range all {
		byName[a.Name] = a
	}
	for _, name := range in.AlarmNames {
		if _, ok := byName[name]; !ok {
			return resourceNotFound("ResourceNotFound")
		}
	}
	for _, name := range in.AlarmNames {
		for _, other := range all {
			if other.Kind == kindComposite && !containsString(in.AlarmNames, other.Name) && references(other, name) {
				return validationError(fmt.Sprintf("Cannot delete %s as there are composite alarm(s) depending on it.", name))
			}
		}
	}
	now := s.now().UTC()
	for _, name := range in.AlarmNames {
		if err := s.metadata.Delete(alarmsBucket, name); err != nil {
			return internal(err)
		}
		if err := s.appendHistory(historyItem{AlarmName: name, AlarmType: byName[name].Kind, Timestamp: now,
			ItemType: "ConfigurationUpdate", Summary: fmt.Sprintf("Alarm %q deleted", name)}); err != nil {
			return err
		}
	}
	return respond(w, r, "DeleteAlarms", requestID, nil)
}

type setAlarmStateInput struct {
	AlarmName       string `json:"AlarmName"`
	StateValue      string `json:"StateValue"`
	StateReason     string `json:"StateReason"`
	StateReasonData string `json:"StateReasonData"`
}

func (s *Service) setAlarmState(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in setAlarmStateInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	if in.AlarmName == "" {
		return missing("AlarmName")
	}
	if in.StateReason == "" {
		return missing("StateReason")
	}
	if !validState(in.StateValue) {
		return invalidValue(in.StateValue, "StateValue")
	}
	if in.StateReasonData != "" && !json.Valid([]byte(in.StateReasonData)) {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidFormat"}
	}
	a, err := s.loadAlarm(in.AlarmName)
	if err != nil {
		return err
	}
	if a == nil {
		return resourceNotFound("ResourceNotFound")
	}
	now := s.now().UTC()
	a.setState(alarmState(in.StateValue), in.StateReason, in.StateReasonData, now)
	if a.Kind == kindMetric && a.Period > 0 {
		// The forced state holds until the evaluator next runs, in a later period.
		a.LastEvaluatedBucket = periodStart(now, a.Period)
	}
	if err := s.commit([]*alarm{a}); err != nil {
		return err
	}
	return respond(w, r, "SetAlarmState", requestID, nil)
}

func (s *Service) toggleActions(w http.ResponseWriter, r *http.Request, requestID string, enabled bool) error {
	var in alarmNamesInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	for _, name := range in.AlarmNames {
		a, err := s.loadAlarm(name)
		if err != nil {
			return err
		}
		if a == nil {
			continue
		}
		a.ActionsEnabled = enabled
		if err := s.storeAlarm(*a); err != nil {
			return err
		}
	}
	action := "DisableAlarmActions"
	if enabled {
		action = "EnableAlarmActions"
	}
	return respond(w, r, action, requestID, nil)
}

// ---- DescribeAlarmHistory ----

type describeHistoryInput struct {
	AlarmName       string   `json:"AlarmName"`
	AlarmTypes      []string `json:"AlarmTypes"`
	HistoryItemType string   `json:"HistoryItemType"`
	StartDate       flexTime `json:"StartDate"`
	EndDate         flexTime `json:"EndDate"`
	MaxRecords      *flexInt `json:"MaxRecords"`
	NextToken       string   `json:"NextToken"`
	ScanBy          string   `json:"ScanBy"`
}

type historyOut struct {
	AlarmName       string    `json:"AlarmName"`
	AlarmType       alarmKind `json:"AlarmType"`
	Timestamp       string    `json:"Timestamp"`
	HistoryItemType string    `json:"HistoryItemType"`
	HistorySummary  string    `json:"HistorySummary"`
	HistoryData     string    `json:"HistoryData"`
}

func (s *Service) describeAlarmHistory(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in describeHistoryInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	switch in.HistoryItemType {
	case "", "ConfigurationUpdate", "StateUpdate", "Action", "AlarmContributorStateUpdate", "AlarmContributorAction":
	default:
		return invalidValue(in.HistoryItemType, "HistoryItemType")
	}
	var start, end time.Time
	if raw := in.StartDate.String(); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			return invalidValue(raw, "StartDate")
		}
		start = t
	}
	if raw := in.EndDate.String(); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			return invalidValue(raw, "EndDate")
		}
		end = t
	}
	if err := s.evaluateDue(); err != nil {
		return err
	}
	var items []historyItem
	err := s.metadata.Scan(historyBucket, "", func(_, v []byte) error {
		var h historyItem
		if json.Unmarshal(v, &h) != nil {
			return nil
		}
		switch {
		case in.AlarmName != "" && h.AlarmName != in.AlarmName:
		case in.HistoryItemType != "" && h.ItemType != in.HistoryItemType:
		case len(in.AlarmTypes) > 0 && !containsString(in.AlarmTypes, string(h.AlarmType)):
		case !start.IsZero() && h.Timestamp.Before(start):
		case !end.IsZero() && h.Timestamp.After(end):
		default:
			items = append(items, h)
		}
		return nil
	})
	if err != nil {
		return internal(err)
	}
	ascending := in.ScanBy == "TimestampAscending"
	sort.SliceStable(items, func(i, j int) bool {
		if ascending {
			return items[i].Timestamp.Before(items[j].Timestamp)
		}
		return items[i].Timestamp.After(items[j].Timestamp)
	})
	page, next, err := paginate(items, in.MaxRecords, in.NextToken, 100)
	if err != nil {
		return err
	}
	out := make([]historyOut, 0, len(page))
	for _, h := range page {
		out = append(out, historyOut{AlarmName: h.AlarmName, AlarmType: h.AlarmType, Timestamp: stamp(h.Timestamp),
			HistoryItemType: h.ItemType, HistorySummary: h.Summary, HistoryData: h.Data})
	}
	return respond(w, r, "DescribeAlarmHistory", requestID, struct {
		AlarmHistoryItems []historyOut `json:"AlarmHistoryItems"`
		NextToken         string       `json:"NextToken,omitempty"`
	}{out, next})
}

// ---- tags ----

type tagResourceInput struct {
	ResourceARN string   `json:"ResourceARN"`
	Tags        []tagIO  `json:"Tags"`
	TagKeys     []string `json:"TagKeys"`
}

func (s *Service) alarmByARN(arn string) (*alarm, error) {
	if arn == "" {
		return nil, missing("ResourceARN")
	}
	if !strings.Contains(arn, ":alarm:") {
		return nil, invalidValue(arn, "ResourceARN")
	}
	a, err := s.loadAlarm(alarmNameOf(arn))
	if err != nil {
		return nil, err
	}
	if a == nil || a.ARN != arn {
		return nil, resourceNotFound("ResourceNotFoundException")
	}
	return a, nil
}

func (s *Service) tagResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in tagResourceInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	a, err := s.alarmByARN(in.ResourceARN)
	if err != nil {
		return err
	}
	merged := a.Tags.Merge(tagList(in.Tags))
	if err := merged.Validate(tags.MaxPerResource); err != nil {
		return validationError(err.Error())
	}
	a.Tags = merged
	if err := s.storeAlarm(*a); err != nil {
		return err
	}
	return respond(w, r, "TagResource", requestID, nil)
}

func (s *Service) untagResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in tagResourceInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	a, err := s.alarmByARN(in.ResourceARN)
	if err != nil {
		return err
	}
	a.Tags = a.Tags.Without(in.TagKeys)
	if err := s.storeAlarm(*a); err != nil {
		return err
	}
	return respond(w, r, "UntagResource", requestID, nil)
}

func (s *Service) listTagsForResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	var in tagResourceInput
	if err := decodeInput(r, &in); err != nil {
		return err
	}
	a, err := s.alarmByARN(in.ResourceARN)
	if err != nil {
		return err
	}
	out := []tagIO{}
	for _, t := range a.Tags {
		out = append(out, tagIO{t.Key, t.Value})
	}
	return respond(w, r, "ListTagsForResource", requestID, struct {
		Tags []tagIO `json:"Tags"`
	}{out})
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
