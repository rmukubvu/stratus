package scheduler

import (
	"strings"
	"testing"
	"time"
)

var anchor = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func mustParse(t *testing.T, expr string, loc *time.Location) Expression {
	t.Helper()
	parsed, err := ParseExpression(expr, loc, anchor)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return parsed
}

func TestRateCountsFromItsAnchor(t *testing.T) {
	expr := mustParse(t, "rate(5 minutes)", time.UTC)
	if expr.Recurring() != true {
		t.Fatal("rate schedules recur")
	}
	next, _ := expr.Next(anchor)
	if !next.Equal(anchor.Add(5 * time.Minute)) {
		t.Fatalf("first run should be one interval after the anchor, got %s", next)
	}
	next, _ = expr.Next(anchor.Add(12 * time.Minute))
	if !next.Equal(anchor.Add(15 * time.Minute)) {
		t.Fatalf("runs stay on the anchor grid, got %s", next)
	}
	next, _ = expr.Next(anchor.Add(-time.Hour))
	if !next.Equal(anchor) {
		t.Fatalf("a future anchor is the first run, got %s", next)
	}
	for _, unit := range []string{"rate(1 hour)", "rate(2 hours)", "rate(1 day)", "rate(3 days)", "RATE( 1 minute )"} {
		mustParse(t, unit, time.UTC)
	}
	for _, bad := range []string{"rate(0 minutes)", "rate(-1 hours)", "rate(5 weeks)", "rate(five minutes)", "rate(5)"} {
		if _, err := ParseExpression(bad, time.UTC, anchor); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestAtRunsOnceInTheScheduleZone(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	expr := mustParse(t, "at(2026-11-10T09:00:00)", paris)
	if expr.Recurring() {
		t.Fatal("at schedules run once")
	}
	next, ok := expr.Next(anchor)
	if !ok || !next.Equal(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("09:00 Paris is 08:00 UTC in November, got %s %v", next, ok)
	}
	if _, ok := expr.Next(next); ok {
		t.Fatal("nothing runs after the one-time instant")
	}
	for _, bad := range []string{"at(2026-11-10)", "at(2026-11-10T09:00:00Z)", "at(tomorrow)"} {
		if _, err := ParseExpression(bad, time.UTC, anchor); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestCronParsesNamesRangesListsAndSteps(t *testing.T) {
	cases := map[string]time.Time{
		"cron(0 9 * * ? *)":          time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),  // daily, next is tomorrow 09:00
		"cron(30 14 ? * MON-FRI *)":  time.Date(2026, 11, 9, 14, 30, 0, 0, time.UTC), // today is a Monday
		"cron(0 0 ? * SUN *)":        time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
		"cron(0 0 ? * 1 *)":          time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), // AWS day-of-week 1 is Sunday
		"cron(0/15 * * * ? *)":       time.Date(2026, 11, 9, 12, 15, 0, 0, time.UTC),
		"cron(5,35 12 * * ? *)":      time.Date(2026, 11, 9, 12, 5, 0, 0, time.UTC),
		"cron(0 8 1 JAN,JUL ? *)":    time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC),
		"cron(0 8 15 * ? 2027)":      time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC),
		"cron(0 8 15 * ? 2027-2030)": time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC),
		"cron(0 */6 * * ? *)":        time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC),
		"cron(0 10-12/2 * * ? *)":    time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC),
		"cron(0 9 ? dec sat *)":      time.Date(2026, 12, 5, 9, 0, 0, 0, time.UTC),
	}
	for expr, want := range cases {
		next, ok := mustParse(t, expr, time.UTC).Next(anchor)
		if !ok || !next.Equal(want) {
			t.Errorf("%s: next after %s = %s (%v), want %s", expr, anchor, next, ok, want)
		}
	}
}

func TestCronHonoursTheScheduleZoneAcrossDST(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	expr := mustParse(t, "cron(0 9 * * ? *)", london)
	winter, _ := expr.Next(time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC))
	if !winter.Equal(time.Date(2026, 3, 28, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("09:00 London is 09:00 UTC before the clocks change, got %s", winter.UTC())
	}
	summer, _ := expr.Next(winter)
	if !summer.Equal(time.Date(2026, 3, 29, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("09:00 London is 08:00 UTC after the clocks change, got %s", summer.UTC())
	}
}

func TestCronStopsWhenADateNeverComes(t *testing.T) {
	if _, ok := mustParse(t, "cron(0 0 31 2 ? *)", time.UTC).Next(anchor); ok {
		t.Fatal("February 31st never happens")
	}
	if _, ok := mustParse(t, "cron(0 0 1 1 ? 2000)", time.UTC).Next(anchor); ok {
		t.Fatal("a past year has no next run")
	}
}

func TestCronRejectsWhatStratusDoesNotSupport(t *testing.T) {
	bad := map[string]string{
		"cron(0 9 * * * *)":       "exactly one of day-of-month and day-of-week",
		"cron(0 9 ? * ? *)":       "exactly one of day-of-month and day-of-week",
		"cron(0 9 L * ? *)":       "L",
		"cron(0 9 ? * 5L *)":      "L",
		"cron(0 9 15W * ? *)":     "W",
		"cron(0 9 ? * 6#3 *)":     "#",
		"cron(0 9 * *)":           "six fields",
		"cron(60 9 * * ? *)":      "between 0 and 59",
		"cron(0 24 * * ? *)":      "between 0 and 23",
		"cron(0 9 0 * ? *)":       "between 1 and 31",
		"cron(0 9 * 13 ? *)":      "between 1 and 12",
		"cron(0 9 ? * 8 *)":       "between 1 and 7",
		"cron(0 9 * * ? 1969)":    "between 1970 and 2199",
		"cron(0 9 * * ? *) extra": "at(...), rate(...) or cron(...)",
		"cron(0 12-9 * * ? *)":    "descending",
		"cron(*/0 * * * ? *)":     "invalid step",
		"every day at nine":       "at(...), rate(...) or cron(...)",
	}
	for expr, want := range bad {
		_, err := ParseExpression(expr, time.UTC, anchor)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error mentioning %q, got %v", expr, want, err)
		}
	}
	// Month and day names contain L and W but are not the L/W modifiers.
	mustParse(t, "cron(0 9 ? JUL WED *)", time.UTC)
}
