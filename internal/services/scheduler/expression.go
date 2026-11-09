package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // schedules name IANA zones; embed them so Windows and slim images resolve them
)

// Expression is a parsed ScheduleExpression: at(...), rate(...) or cron(...).
type Expression interface {
	// Next returns the earliest invocation strictly after t, or false when
	// the schedule has nothing further to run.
	Next(t time.Time) (time.Time, bool)
	// Recurring is false for one-time at() schedules.
	Recurring() bool
}

// ParseExpression parses expr. loc is the schedule's timezone and anchor is
// the moment rate() schedules count from: the start date when one is set,
// otherwise the creation time.
func ParseExpression(expr string, loc *time.Location, anchor time.Time) (Expression, error) {
	kind, arg, ok := splitCall(strings.TrimSpace(expr))
	if !ok {
		return nil, fmt.Errorf("schedule expression must be at(...), rate(...) or cron(...), got %q", expr)
	}
	switch kind {
	case "at":
		return parseAt(arg, loc)
	case "rate":
		return parseRate(arg, anchor)
	case "cron":
		return parseCron(arg, loc)
	default:
		return nil, fmt.Errorf("schedule expression must be at(...), rate(...) or cron(...), got %q", expr)
	}
}

func splitCall(expr string) (kind, arg string, ok bool) {
	open := strings.IndexByte(expr, '(')
	if open <= 0 || !strings.HasSuffix(expr, ")") {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(expr[:open])), strings.TrimSpace(expr[open+1 : len(expr)-1]), true
}

// atExpression runs once, at a wall-clock time in the schedule's zone.
type atExpression struct{ when time.Time }

func parseAt(arg string, loc *time.Location) (Expression, error) {
	when, err := time.ParseInLocation("2006-01-02T15:04:05", arg, loc)
	if err != nil {
		return nil, fmt.Errorf("at() takes a local time such as at(2026-11-10T09:00:00), got %q", arg)
	}
	return atExpression{when: when}, nil
}

func (e atExpression) Next(t time.Time) (time.Time, bool) {
	if e.when.After(t) {
		return e.when, true
	}
	return time.Time{}, false
}

func (atExpression) Recurring() bool { return false }

// rateExpression fires every interval, counted from its anchor. A schedule
// without a start date first fires one interval after creation; one with a
// start date first fires at that date.
type rateExpression struct {
	interval time.Duration
	anchor   time.Time
}

func parseRate(arg string, anchor time.Time) (Expression, error) {
	fields := strings.Fields(arg)
	if len(fields) != 2 {
		return nil, fmt.Errorf("rate() takes a value and a unit such as rate(5 minutes), got %q", arg)
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("rate() value must be a positive integer, got %q", fields[0])
	}
	var unit time.Duration
	switch strings.ToLower(fields[1]) {
	case "minute", "minutes":
		unit = time.Minute
	case "hour", "hours":
		unit = time.Hour
	case "day", "days":
		unit = 24 * time.Hour
	default:
		return nil, fmt.Errorf("rate() unit must be minutes, hours or days, got %q", fields[1])
	}
	return rateExpression{interval: time.Duration(n) * unit, anchor: anchor}, nil
}

func (e rateExpression) Next(t time.Time) (time.Time, bool) {
	if e.anchor.After(t) {
		return e.anchor, true
	}
	k := t.Sub(e.anchor)/e.interval + 1
	return e.anchor.Add(k * e.interval), true
}

func (rateExpression) Recurring() bool { return true }

// cronExpression is the six-field AWS cron: minutes hours day-of-month
// month day-of-week year. Day-of-week counts 1=SUN … 7=SAT as AWS does.
type cronExpression struct {
	minutes, hours, dayOfMonth, months, dayOfWeek, years fieldSet
	loc                                                  *time.Location
}

const maxCronYear = 2199

var (
	monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
	dayNames   = map[string]int{"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}
)

func parseCron(arg string, loc *time.Location) (Expression, error) {
	fields := strings.Fields(arg)
	if len(fields) != 6 {
		return nil, fmt.Errorf("cron() takes six fields (minutes hours day-of-month month day-of-week year), got %d in %q", len(fields), arg)
	}
	var (
		expr = cronExpression{loc: loc}
		err  error
	)
	if expr.minutes, err = parseField("minutes", fields[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if expr.hours, err = parseField("hours", fields[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if expr.dayOfMonth, err = parseField("day-of-month", fields[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if expr.months, err = parseField("month", fields[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	if expr.dayOfWeek, err = parseField("day-of-week", fields[4], 1, 7, dayNames); err != nil {
		return nil, err
	}
	if expr.years, err = parseField("year", fields[5], 1970, maxCronYear, nil); err != nil {
		return nil, err
	}
	if expr.dayOfMonth.question == expr.dayOfWeek.question {
		return nil, fmt.Errorf("cron() needs ? in exactly one of day-of-month and day-of-week; AWS does not allow both day-of-month and day-of-week to carry a value")
	}
	return expr, nil
}

func (e cronExpression) Next(after time.Time) (time.Time, bool) {
	t := after.In(e.loc).Truncate(time.Minute).Add(time.Minute)
	// Each miss jumps to the start of the next candidate unit, so even a date
	// that never occurs (Feb 31) is rejected within a few thousand steps.
	for steps := 0; steps < 200_000; steps++ {
		year, month, day := t.Date()
		switch {
		case year > maxCronYear:
			return time.Time{}, false
		case !e.years.has(year):
			t = time.Date(year+1, time.January, 1, 0, 0, 0, 0, e.loc)
		case !e.months.has(int(month)):
			t = time.Date(year, month+1, 1, 0, 0, 0, 0, e.loc)
		case !e.dayOfMonth.has(day) || !e.dayOfWeek.has(int(t.Weekday())+1):
			t = time.Date(year, month, day+1, 0, 0, 0, 0, e.loc)
		case !e.hours.has(t.Hour()):
			t = time.Date(year, month, day, t.Hour()+1, 0, 0, 0, e.loc)
		case !e.minutes.has(t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

func (cronExpression) Recurring() bool { return true }

// fieldSet is the set of values one cron field accepts.
type fieldSet struct {
	// any is true for * and ?; question distinguishes ?, which AWS requires
	// in exactly one of the two day fields.
	any      bool
	question bool
	values   map[int]bool
}

func (f fieldSet) has(v int) bool { return f.any || f.values[v] }

func parseField(name, spec string, min, max int, names map[string]int) (fieldSet, error) {
	if spec == "*" || spec == "?" {
		return fieldSet{any: true, question: spec == "?"}, nil
	}
	upper := strings.ToUpper(spec)
	for _, unsupported := range []string{"L", "W", "#"} {
		if strings.Contains(stripNames(upper, names), unsupported) {
			return fieldSet{}, fmt.Errorf("cron %s field %q uses %s, which Stratus does not support yet", name, spec, unsupported)
		}
	}
	set := fieldSet{values: map[int]bool{}}
	for _, item := range strings.Split(upper, ",") {
		if err := addCronItem(set.values, name, item, min, max, names); err != nil {
			return fieldSet{}, err
		}
	}
	return set, nil
}

// stripNames removes month and day names so the L/W/# check does not trip
// on JUL or WED.
func stripNames(spec string, names map[string]int) string {
	for name := range names {
		spec = strings.ReplaceAll(spec, name, "")
	}
	return spec
}

func addCronItem(values map[int]bool, field, item string, min, max int, names map[string]int) error {
	rangeSpec, stepSpec, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepSpec)
		if err != nil || n <= 0 {
			return fmt.Errorf("cron %s field %q has an invalid step", field, item)
		}
		step = n
	}
	lo, hi := min, max
	switch {
	case rangeSpec == "*" || rangeSpec == "?":
	case strings.Contains(rangeSpec, "-"):
		from, to, _ := strings.Cut(rangeSpec, "-")
		var err error
		if lo, err = cronValue(field, from, min, max, names); err != nil {
			return err
		}
		if hi, err = cronValue(field, to, min, max, names); err != nil {
			return err
		}
		if lo > hi {
			return fmt.Errorf("cron %s field %q is a descending range", field, item)
		}
	default:
		v, err := cronValue(field, rangeSpec, min, max, names)
		if err != nil {
			return err
		}
		lo = v
		if !hasStep {
			hi = v
		}
	}
	for v := lo; v <= hi; v += step {
		values[v] = true
	}
	return nil
}

func cronValue(field, raw string, min, max int, names map[string]int) (int, error) {
	if v, ok := names[raw]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("cron %s field value %q must be between %d and %d", field, raw, min, max)
	}
	return v, nil
}
