package monitoring

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// verdict is the outcome of evaluating a metric alarm: the state it should
// be in, with CloudWatch's reason text and reason data. keep is true when
// the alarm should stay as it is (TreatMissingData=ignore with no data).
type verdict struct {
	state      alarmState
	reason     string
	reasonData string
	keep       bool
}

// bucket is one period of the evaluation window and its statistic, if any
// datapoints fell in it.
type bucket struct {
	start   time.Time
	value   float64
	samples int
	present bool
}

// periodStart aligns t down to the start of its period, as CloudWatch
// aligns periods to the epoch.
func periodStart(t time.Time, period int) time.Time {
	p := int64(period)
	return time.Unix(t.Unix()/p*p, 0).UTC()
}

// windowBuckets groups the alarm's datapoints into its EvaluationPeriods
// periods, oldest first. Like CloudWatch's evaluator, it looks only at
// complete periods: the newest is the one before now's.
func windowBuckets(a alarm, points []metricDatum, now time.Time) []bucket {
	n, period := a.EvaluationPeriods, a.Period
	newest := periodStart(now, period).Add(-time.Duration(period) * time.Second)
	buckets := make([]bucket, n)
	values := make([][]float64, n)
	for i := range buckets {
		buckets[i].start = newest.Add(-time.Duration((n-1-i)*period) * time.Second)
	}
	oldest := buckets[0].start
	for _, p := range points {
		if p.Namespace != a.Namespace || p.MetricName != a.MetricName || !sameDimensions(sortedDimensions(p.Dimensions), sortedDimensions(a.Dimensions)) {
			continue
		}
		if a.Unit != "" && p.Unit != a.Unit {
			continue
		}
		start := periodStart(p.Timestamp, period)
		if start.Before(oldest) || start.After(newest) {
			continue
		}
		i := int(start.Sub(oldest) / (time.Duration(period) * time.Second))
		values[i] = append(values[i], p.Value)
	}
	for i, v := range values {
		if len(v) == 0 {
			continue
		}
		stat, ok := statistic(a, v)
		if !ok {
			continue
		}
		buckets[i].value, buckets[i].samples, buckets[i].present = stat, len(v), true
	}
	return buckets
}

// statistic computes the alarm's statistic over one period's values. It is
// false for a percentile CloudWatch would not evaluate on too few samples
// (EvaluateLowSampleCountPercentile=ignore).
func statistic(a alarm, values []float64) (float64, bool) {
	switch a.Statistic {
	case "Average":
		return sum(values) / float64(len(values)), true
	case "Sum":
		return sum(values), true
	case "Minimum":
		return minOf(values), true
	case "Maximum":
		return maxOf(values), true
	case "SampleCount":
		return float64(len(values)), true
	}
	p, ok := percentile(a.ExtendedStatistic)
	if !ok {
		return 0, false
	}
	if a.EvaluateLowSamples == "ignore" && p < 100 && float64(len(values)) < 10/(1-p/100) {
		return 0, false
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	return sorted[rank], true
}

// percentile parses "p90" or "p99.9".
func percentile(stat string) (float64, bool) {
	if !strings.HasPrefix(stat, "p") {
		return 0, false
	}
	p, err := strconv.ParseFloat(stat[1:], 64)
	return p, err == nil && p >= 0 && p <= 100
}

func breaches(a alarm, v float64) bool {
	t := *a.Threshold
	switch a.ComparisonOperator {
	case "GreaterThanThreshold":
		return v > t
	case "GreaterThanOrEqualToThreshold":
		return v >= t
	case "LessThanThreshold":
		return v < t
	case "LessThanOrEqualToThreshold":
		return v <= t
	}
	return false
}

var relations = map[string]string{
	"GreaterThanThreshold":          "greater than",
	"GreaterThanOrEqualToThreshold": "greater than or equal to",
	"LessThanThreshold":             "less than",
	"LessThanOrEqualToThreshold":    "less than or equal to",
}

// evaluate decides a metric alarm's state from the datapoints in its window.
// An alarm goes to ALARM when at least DatapointsToAlarm of the evaluated
// periods breach, to OK otherwise, and to INSUFFICIENT_DATA when no period
// has data and missing data is not treated as (non-)breaching.
func evaluate(a alarm, points []metricDatum, now time.Time) verdict {
	buckets := windowBuckets(a, points, now)
	m := a.DatapointsToAlarm
	if m == 0 {
		m = a.EvaluationPeriods
	}
	treat := a.TreatMissingData
	if treat == "" {
		treat = "missing"
	}

	var present []bucket
	for _, b := range buckets {
		if b.present {
			present = append(present, b)
		}
	}
	missing := len(buckets) - len(present)
	data := reasonData(a, buckets, nil, now)

	if len(present) == 0 {
		switch treat {
		case "breaching", "notBreaching":
			label, state := "Breaching", stateAlarm
			if treat == "notBreaching" {
				label, state = "NonBreaching", stateOK
			}
			return verdict{state: state, reasonData: data, reason: fmt.Sprintf(
				"Threshold Crossed: no datapoints were received for %s and %s treated as [%s].",
				plural(missing, "period", "periods"), plural(missing, "missing datapoint was", "missing datapoints were"), label)}
		case "ignore":
			return verdict{keep: true}
		default:
			return verdict{state: stateInsufficientData, reasonData: data, reason: fmt.Sprintf(
				"Insufficient Data: %s unknown.", plural(missing, "datapoint was", "datapoints were"))}
		}
	}

	breaching, inRange := []bucket{}, []bucket{}
	for _, b := range present {
		if breaches(a, b.value) {
			breaching = append(breaching, b)
		} else {
			inRange = append(inRange, b)
		}
	}
	extraBreaches := 0
	if treat == "breaching" {
		extraBreaches = missing
	}
	if len(breaching)+extraBreaches >= m {
		cited := newest(breaching, m)
		return verdict{state: stateAlarm, reasonData: reasonData(a, buckets, cited, now), reason: thresholdReason(a, cited, len(buckets), m, true)}
	}
	okMinimum := len(buckets) - m + 1
	cited := newest(inRange, okMinimum)
	return verdict{state: stateOK, reasonData: reasonData(a, buckets, cited, now), reason: thresholdReason(a, cited, len(buckets), okMinimum, false)}
}

// newest returns up to n of the buckets, newest first: the datapoints
// CloudWatch cites as deciding a transition.
func newest(buckets []bucket, n int) []bucket {
	out := append([]bucket(nil), buckets...)
	sort.Slice(out, func(i, j int) bool { return out[i].start.After(out[j].start) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// thresholdReason renders CloudWatch's "Threshold Crossed" reason, citing the
// datapoints (newest first) that decided the transition.
func thresholdReason(a alarm, cited []bucket, n, minimum int, toAlarm bool) string {
	relation := relations[a.ComparisonOperator]
	if !toAlarm {
		relation = "not " + relation
	}
	parts := make([]string, len(cited))
	for i, b := range cited {
		parts[i] = javaDouble(b.value) + " (" + b.start.Format("02/01/06 15:04:05") + ")"
	}
	verb := "were"
	if len(cited) == 1 {
		verb = "was"
	}
	threshold := javaDouble(*a.Threshold)
	transition := "OK -> ALARM"
	if !toAlarm {
		transition = "ALARM -> OK"
	}
	return fmt.Sprintf("Threshold Crossed: %d out of the last %d datapoints [%s] %s %s the threshold (%s) (minimum %s for %s transition).",
		len(cited), n, strings.Join(parts, ", "), verb, relation, threshold, plural(minimum, "datapoint", "datapoints"), transition)
}

// reasonData is the StateReasonData JSON: every value in the window as
// recentDatapoints, and the datapoints the reason cites as evaluatedDatapoints.
func reasonData(a alarm, buckets, cited []bucket, now time.Time) string {
	type point struct {
		Timestamp   string  `json:"timestamp"`
		SampleCount float64 `json:"sampleCount"`
		Value       float64 `json:"value"`
	}
	recent := []float64{}
	evaluated := []point{}
	for _, b := range buckets {
		if b.present {
			recent = append(recent, b.value)
		}
	}
	for _, b := range cited {
		evaluated = append(evaluated, point{Timestamp: b.start.Format(isoMillis), SampleCount: float64(b.samples), Value: b.value})
	}
	stat := a.Statistic
	if stat == "" {
		stat = a.ExtendedStatistic
	}
	raw, _ := json.Marshal(map[string]any{
		"version":             "1.0",
		"queryDate":           now.UTC().Format(isoMillis),
		"startDate":           buckets[0].start.Format(isoMillis),
		"statistic":           stat,
		"period":              a.Period,
		"recentDatapoints":    recent,
		"threshold":           *a.Threshold,
		"evaluatedDatapoints": evaluated,
	})
	return string(raw)
}

const isoMillis = "2006-01-02T15:04:05.000+0000"

// javaDouble renders v as CloudWatch (Java's Double.toString) does: 100.0.
func javaDouble(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func sum(v []float64) float64 {
	total := 0.0
	for _, x := range v {
		total += x
	}
	return total
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		m = math.Min(m, x)
	}
	return m
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		m = math.Max(m, x)
	}
	return m
}

func sortedDimensions(d []dimension) []dimension {
	out := append([]dimension(nil), d...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
