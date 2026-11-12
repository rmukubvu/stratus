package monitoring

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store/bbolt"
)

// alarmFixture drives the service over AWS JSON with a controllable clock.
type alarmFixture struct {
	t     *testing.T
	svc   *Service
	clock time.Time
	sns   *recordingPublisher
}

type recordingPublisher struct{ messages map[string][]string }

func (p *recordingPublisher) PublishToTopic(arn, message string) error {
	p.messages[arn] = append(p.messages[arn], message)
	return nil
}

func newAlarmFixture(t *testing.T) *alarmFixture {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	f := &alarmFixture{t: t, svc: NewService(meta), clock: time.Date(2026, 11, 12, 10, 0, 5, 0, time.UTC),
		sns: &recordingPublisher{messages: map[string][]string{}}}
	f.svc.now = func() time.Time { return f.clock }
	f.svc.SetScope(scope.Scope{Account: "111111111111", Region: "eu-west-1"})
	f.svc.SetSNS(f.sns)
	return f
}

func (f *alarmFixture) call(op string, input any) (map[string]any, error) {
	f.t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")
	r.Header.Set("X-Amz-Target", "GraniteServiceVersion20100801."+op)
	w := httptest.NewRecorder()
	if err := f.svc.Handle(w, r, op, "req-1"); err != nil {
		return nil, err
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, nil
}

func (f *alarmFixture) must(op string, input any) map[string]any {
	f.t.Helper()
	out, err := f.call(op, input)
	if err != nil {
		f.t.Fatalf("%s: %v", op, err)
	}
	return out
}

func (f *alarmFixture) mustFail(code, message, op string, input any) {
	f.t.Helper()
	_, err := f.call(op, input)
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code || (message != "" && apiErr.Message != message) {
		f.t.Fatalf("%s: want %s %q, got %v", op, code, message, err)
	}
}

func (f *alarmFixture) alarm(name string) map[string]any {
	f.t.Helper()
	out := f.must("DescribeAlarms", map[string]any{"AlarmNames": []string{name}, "AlarmTypes": []string{"MetricAlarm", "CompositeAlarm"}})
	for _, key := range []string{"MetricAlarms", "CompositeAlarms"} {
		if list, _ := out[key].([]any); len(list) == 1 {
			return list[0].(map[string]any)
		}
	}
	f.t.Fatalf("alarm %s not found: %v", name, out)
	return nil
}

func (f *alarmFixture) put(value float64, at time.Time) {
	f.t.Helper()
	f.must("PutMetricData", map[string]any{"Namespace": "App", "MetricData": []map[string]any{
		{"MetricName": "Latency", "Value": value, "Timestamp": at.Format(time.RFC3339)},
	}})
}

func (f *alarmFixture) history(name string) []map[string]any {
	f.t.Helper()
	out := f.must("DescribeAlarmHistory", map[string]any{"AlarmName": name, "HistoryItemType": "StateUpdate", "ScanBy": "TimestampAscending"})
	var items []map[string]any
	for _, item := range out["AlarmHistoryItems"].([]any) {
		items = append(items, item.(map[string]any))
	}
	return items
}

func latencyAlarm(name string, extra map[string]any) map[string]any {
	in := map[string]any{
		"AlarmName": name, "Namespace": "App", "MetricName": "Latency", "Statistic": "Maximum",
		"Period": 60, "EvaluationPeriods": 2, "Threshold": 100, "ComparisonOperator": "GreaterThanThreshold",
	}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func TestAlarmEvaluatesCompletedPeriods(t *testing.T) {
	f := newAlarmFixture(t)
	topic := "arn:aws:sns:eu-west-1:111111111111:pager"
	f.must("PutMetricAlarm", latencyAlarm("slow", map[string]any{"AlarmActions": []string{topic}}))

	a := f.alarm("slow")
	if a["StateValue"] != "INSUFFICIENT_DATA" || a["StateReason"] != "Unchecked: Initial alarm creation" {
		t.Fatalf("new alarm: %v", a)
	}
	if a["AlarmArn"] != "arn:aws:cloudwatch:eu-west-1:111111111111:alarm:slow" {
		t.Fatalf("arn %v", a["AlarmArn"])
	}

	// Two breaching minutes; the evaluator sees them once both have completed.
	f.put(150, f.clock)
	f.clock = f.clock.Add(time.Minute)
	f.put(180, f.clock)
	if got := f.alarm("slow")["StateValue"]; got == "ALARM" {
		t.Fatalf("one breaching period of two must not alarm: %v", got)
	}
	f.clock = f.clock.Add(time.Minute)
	a = f.alarm("slow")
	want := "Threshold Crossed: 2 out of the last 2 datapoints [180.0 (12/11/26 10:01:00), 150.0 (12/11/26 10:00:00)] " +
		"were greater than the threshold (100.0) (minimum 2 datapoints for OK -> ALARM transition)."
	if a["StateValue"] != "ALARM" || a["StateReason"] != want {
		t.Fatalf("after breaches: %v / %v", a["StateValue"], a["StateReason"])
	}
	if len(f.sns.messages[topic]) != 1 || !strings.Contains(f.sns.messages[topic][0], `"NewStateValue":"ALARM"`) {
		t.Fatalf("notification: %v", f.sns.messages)
	}

	history := f.history("slow")
	history = history[len(history)-1:]
	if history[0]["HistorySummary"] != "Alarm updated from OK to ALARM" {
		t.Fatalf("history: %v", history)
	}
	var data struct {
		OldState struct{ StateValue string } `json:"oldState"`
		NewState struct {
			StateValue      string `json:"stateValue"`
			StateReasonData struct {
				RecentDatapoints    []float64        `json:"recentDatapoints"`
				EvaluatedDatapoints []map[string]any `json:"evaluatedDatapoints"`
			} `json:"stateReasonData"`
		} `json:"newState"`
	}
	if err := json.Unmarshal([]byte(history[0]["HistoryData"].(string)), &data); err != nil {
		t.Fatal(err)
	}
	if data.NewState.StateValue != "ALARM" || len(data.NewState.StateReasonData.RecentDatapoints) != 2 || len(data.NewState.StateReasonData.EvaluatedDatapoints) != 2 {
		t.Fatalf("history data: %+v", data)
	}
}

func TestTreatMissingData(t *testing.T) {
	cases := map[string]string{"breaching": "ALARM", "notBreaching": "OK", "missing": "INSUFFICIENT_DATA", "ignore": "INSUFFICIENT_DATA"}
	for treat, want := range cases {
		t.Run(treat, func(t *testing.T) {
			f := newAlarmFixture(t)
			f.must("PutMetricAlarm", latencyAlarm("a", map[string]any{"EvaluationPeriods": 1, "TreatMissingData": treat}))
			f.clock = f.clock.Add(2 * time.Minute)
			if got := f.alarm("a")["StateValue"]; got != want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestSetAlarmStateHoldsUntilTheNextEvaluation(t *testing.T) {
	f := newAlarmFixture(t)
	f.must("PutMetricAlarm", latencyAlarm("a", map[string]any{"EvaluationPeriods": 1, "TreatMissingData": "notBreaching"}))
	f.must("SetAlarmState", map[string]any{"AlarmName": "a", "StateValue": "ALARM", "StateReason": "forced"})
	if a := f.alarm("a"); a["StateValue"] != "ALARM" || a["StateReason"] != "forced" {
		t.Fatalf("forced: %v", a)
	}
	f.clock = f.clock.Add(time.Minute)
	if got := f.alarm("a")["StateValue"]; got != "OK" {
		t.Fatalf("evaluation should reclaim the forced state, got %v", got)
	}

	f.mustFail("InvalidFormat", "", "SetAlarmState", map[string]any{"AlarmName": "a", "StateValue": "OK", "StateReason": "r", "StateReasonData": "{nope"})
	f.mustFail("ResourceNotFound", "", "SetAlarmState", map[string]any{"AlarmName": "absent", "StateValue": "OK", "StateReason": "r"})
	f.mustFail("InvalidParameterValue", "", "SetAlarmState", map[string]any{"AlarmName": "a", "StateValue": "BROKEN", "StateReason": "r"})
}

func TestPutMetricAlarmValidation(t *testing.T) {
	f := newAlarmFixture(t)
	f.mustFail("ValidationError", "DatapointsToAlarm must be less than or equal to EvaluationPeriods", "PutMetricAlarm",
		latencyAlarm("a", map[string]any{"DatapointsToAlarm": 3}))
	f.mustFail("ValidationError", "Period must be 10, 20, 30 or a multiple of 60", "PutMetricAlarm", latencyAlarm("a", map[string]any{"Period": 45}))
	f.mustFail("InvalidParameterCombination", "", "PutMetricAlarm", latencyAlarm("a", map[string]any{"ExtendedStatistic": "p99"}))
	f.mustFail("MissingParameter", "", "PutMetricAlarm", latencyAlarm("a", map[string]any{"EvaluationPeriods": nil}))
	f.mustFail("InvalidParameterValue", "", "PutMetricAlarm", latencyAlarm("a", map[string]any{"ComparisonOperator": "Bigger"}))
}

func TestCompositeAlarms(t *testing.T) {
	f := newAlarmFixture(t)
	for _, name := range []string{"cpu", "mem"} {
		f.must("PutMetricAlarm", latencyAlarm(name, nil))
	}
	f.mustFail("ValidationError", "Could not save the composite alarm as alarms [disk] in the alarm rule do not exist",
		"PutCompositeAlarm", map[string]any{"AlarmName": "both", "AlarmRule": `ALARM("cpu") AND ALARM(disk)`})
	f.must("PutCompositeAlarm", map[string]any{"AlarmName": "both", "AlarmRule": `ALARM("cpu") AND ALARM("mem")`})
	f.must("PutCompositeAlarm", map[string]any{"AlarmName": "either", "AlarmRule": `ALARM("cpu") OR ALARM("mem")`})
	if got := f.alarm("both")["StateValue"]; got != "OK" {
		t.Fatalf("composite with OK rule: %v", got)
	}

	f.must("SetAlarmState", map[string]any{"AlarmName": "cpu", "StateValue": "ALARM", "StateReason": "test"})
	if got := f.alarm("either"); got["StateValue"] != "ALARM" ||
		!strings.HasPrefix(got["StateReason"].(string), "arn:aws:cloudwatch:eu-west-1:111111111111:alarm:cpu transitioned to ALARM at ") {
		t.Fatalf("OR composite: %v", got)
	}
	if got := f.alarm("both")["StateValue"]; got != "OK" {
		t.Fatalf("AND composite with one child in ALARM: %v", got)
	}

	f.mustFail("ValidationError", "AlarmName [both] is already used by another alarm of a different type", "PutMetricAlarm", latencyAlarm("both", nil))
	f.mustFail("ValidationError", "Cannot delete cpu as there are composite alarm(s) depending on it.", "DeleteAlarms", map[string]any{"AlarmNames": []string{"cpu"}})
	children := f.must("DescribeAlarms", map[string]any{"ChildrenOfAlarmName": "both"})
	if len(children["MetricAlarms"].([]any)) != 2 {
		t.Fatalf("children: %v", children)
	}
	f.must("DeleteAlarms", map[string]any{"AlarmNames": []string{"both", "either", "cpu"}})
	f.mustFail("ResourceNotFound", "", "DeleteAlarms", map[string]any{"AlarmNames": []string{"cpu"}})
}

func TestDescribeAlarmsDefaultsToMetricAlarmsAndPaginates(t *testing.T) {
	f := newAlarmFixture(t)
	for _, name := range []string{"a1", "a2", "a3"} {
		f.must("PutMetricAlarm", latencyAlarm(name, nil))
	}
	f.must("PutCompositeAlarm", map[string]any{"AlarmName": "c", "AlarmRule": `ALARM(a1)`})
	page := f.must("DescribeAlarms", map[string]any{"MaxRecords": 2})
	if len(page["MetricAlarms"].([]any)) != 2 || len(page["CompositeAlarms"].([]any)) != 0 || page["NextToken"] == nil {
		t.Fatalf("first page: %v", page)
	}
	rest := f.must("DescribeAlarms", map[string]any{"MaxRecords": 2, "NextToken": page["NextToken"]})
	if len(rest["MetricAlarms"].([]any)) != 1 || rest["NextToken"] != nil {
		t.Fatalf("second page: %v", rest)
	}
	f.mustFail("InvalidNextToken", "", "DescribeAlarms", map[string]any{"NextToken": "bogus"})

	forMetric := f.must("DescribeAlarmsForMetric", map[string]any{"Namespace": "App", "MetricName": "Latency"})
	names := []string{}
	for _, a := range forMetric["MetricAlarms"].([]any) {
		names = append(names, a.(map[string]any)["AlarmName"].(string))
	}
	if strings.Join(names, ",") != "a1,a2,a3" {
		t.Fatalf("DescribeAlarmsForMetric order: %v", names)
	}
}

func TestAlarmTags(t *testing.T) {
	f := newAlarmFixture(t)
	f.must("PutMetricAlarm", latencyAlarm("a", map[string]any{"Tags": []map[string]string{{"Key": "env", "Value": "dev"}}}))
	arn := f.alarm("a")["AlarmArn"]
	f.must("TagResource", map[string]any{"ResourceARN": arn, "Tags": []map[string]string{{"Key": "team", "Value": "core"}}})
	f.must("UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"env"}})
	out := f.must("ListTagsForResource", map[string]any{"ResourceARN": arn})
	if tags := out["Tags"].([]any); len(tags) != 1 || tags[0].(map[string]any)["Key"] != "team" {
		t.Fatalf("tags: %v", out)
	}
	f.mustFail("ResourceNotFoundException", "", "ListTagsForResource", map[string]any{"ResourceARN": "arn:aws:cloudwatch:eu-west-1:111111111111:alarm:absent"})
}

func TestAlarmOperationsOverTheQueryProtocol(t *testing.T) {
	f := newAlarmFixture(t)
	form := url.Values{"Action": {"PutMetricAlarm"}, "AlarmName": {"q"}, "Namespace": {"App"}, "MetricName": {"Latency"},
		"Statistic": {"Average"}, "Period": {"60"}, "EvaluationPeriods": {"1"}, "Threshold": {"1.5"},
		"ComparisonOperator": {"LessThanThreshold"}, "Dimensions.member.1.Name": {"Service"}, "Dimensions.member.1.Value": {"api"}}
	queryCall := func(form url.Values) string {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		if err := f.svc.Handle(w, r, form.Get("Action"), "req-q"); err != nil {
			t.Fatalf("%s: %v", form.Get("Action"), err)
		}
		return w.Body.String()
	}
	if body := queryCall(form); !strings.Contains(body, "<PutMetricAlarmResponse") || !strings.Contains(body, "<RequestId>req-q</RequestId>") {
		t.Fatalf("PutMetricAlarm: %s", body)
	}
	body := queryCall(url.Values{"Action": {"DescribeAlarms"}, "AlarmNames.member.1": {"q"}})
	for _, want := range []string{
		"<DescribeAlarmsResult><MetricAlarms><member><AlarmName>q</AlarmName>",
		"<Dimensions><member><Name>Service</Name><Value>api</Value></member></Dimensions>",
		"<Threshold>1.5</Threshold>", "<CompositeAlarms></CompositeAlarms>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("DescribeAlarms lacks %q: %s", want, body)
		}
	}
}

func TestAlarmRuleParser(t *testing.T) {
	states := map[string]alarmState{"a": stateAlarm, "b": stateOK, "c": stateInsufficientData}
	stateOf := func(name string) alarmState { return states[alarmNameOf(name)] }
	cases := map[string]bool{
		`ALARM("a")`:                    true,
		`ALARM(a) AND OK(b)`:            true,
		`ALARM(a) AND NOT OK(b)`:        false,
		`OK(b) OR ALARM(b)`:             true,
		`NOT (ALARM(a) OR ALARM(b))`:    false,
		`INSUFFICIENT_DATA(c) and true`: true,
		`FALSE OR ALARM("arn:aws:cloudwatch:us-east-1:1:alarm:a")`: true,
		`ALARM(a) AND OK(b) OR ALARM(c)`:                           true, // AND binds tighter
	}
	for text, want := range cases {
		rule, err := parseAlarmRule(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got := rule.holds(stateOf); got != want {
			t.Errorf("%s = %v", text, got)
		}
	}
	rule, _ := parseAlarmRule(`ALARM(a) OR (OK("b") AND ALARM(a))`)
	if got := strings.Join(ruleChildren(rule), ","); got != "a,b" {
		t.Errorf("children %s", got)
	}
	for _, bad := range []string{``, `ALARM(`, `ALARM(a) AND`, `MAYBE(a)`, `ALARM(a) b`, `ALARM("a)`} {
		if _, err := parseAlarmRule(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestStatistics(t *testing.T) {
	values := []float64{4, 1, 3, 2}
	for stat, want := range map[string]float64{"Average": 2.5, "Sum": 10, "Minimum": 1, "Maximum": 4, "SampleCount": 4} {
		if got, _ := statistic(alarm{Statistic: stat}, values); got != want {
			t.Errorf("%s = %v", stat, got)
		}
	}
	if got, _ := statistic(alarm{ExtendedStatistic: "p50"}, values); got != 2 {
		t.Errorf("p50 = %v", got)
	}
	if _, ok := statistic(alarm{ExtendedStatistic: "p90", EvaluateLowSamples: "ignore"}, values); ok {
		t.Error("p90 over 4 samples should be withheld when low sample counts are ignored")
	}
	if javaDouble(100) != "100.0" || javaDouble(1.25) != "1.25" {
		t.Error("javaDouble")
	}
}

func TestEvaluationHorizonAndMetricMathValidation(t *testing.T) {
	f := newAlarmFixture(t)
	f.must("PutMetricAlarm", latencyAlarm("a", map[string]any{"EvaluationPeriods": 1440}))
	f.mustFail("ValidationError", "Metrics cannot be checked across more than a day (EvaluationPeriods * Period must be <= 86400) for alarms using period < 3600",
		"PutMetricAlarm", latencyAlarm("a", map[string]any{"EvaluationPeriods": 1441}))
	f.mustFail("ValidationError", "Metrics cannot be checked across more than a week (EvaluationPeriods * Period must be <= 604800) for alarms using period >= 3600",
		"PutMetricAlarm", latencyAlarm("a", map[string]any{"Period": 3600, "EvaluationPeriods": 169}))

	math := func(returning ...bool) map[string]any {
		var metrics []map[string]any
		for i, r := range returning {
			metrics = append(metrics, map[string]any{"Id": "m" + string(rune('1'+i)), "Expression": "SUM(METRICS())", "ReturnData": r})
		}
		return map[string]any{"AlarmName": "math", "EvaluationPeriods": 1, "Threshold": 1, "ComparisonOperator": "GreaterThanThreshold", "Metrics": metrics}
	}
	f.mustFail("ValidationError", "Exactly one element of the metrics list should return data.", "PutMetricAlarm", math(false, false))
	f.mustFail("ValidationError", "Exactly one element of the metrics list should return data.", "PutMetricAlarm", math(true, true))
	f.must("PutMetricAlarm", math(false, true))
}

func TestValuesAndCountsFeedStatistics(t *testing.T) {
	f := newAlarmFixture(t)
	f.must("PutMetricAlarm", latencyAlarm("p", map[string]any{"EvaluationPeriods": 1, "Statistic": nil, "ExtendedStatistic": "p50"}))
	f.must("PutMetricData", map[string]any{"Namespace": "App", "MetricData": []map[string]any{
		{"MetricName": "Latency", "Values": []float64{10, 500}, "Counts": []float64{1, 3}, "Timestamp": f.clock.Format(time.RFC3339)},
	}})
	f.clock = f.clock.Add(time.Minute)
	if a := f.alarm("p"); a["StateValue"] != "ALARM" {
		t.Fatalf("p50 of [10, 500, 500, 500] is 500: %v / %v", a["StateValue"], a["StateReason"])
	}
	f.mustFail("InvalidParameterValue", "", "PutMetricData", map[string]any{"Namespace": "App", "MetricData": []map[string]any{
		{"MetricName": "Latency", "Values": []float64{1, 2}, "Counts": []float64{1}},
	}})
}
