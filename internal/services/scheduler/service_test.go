package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/bbolt"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

const queueARN = "arn:aws:sqs:us-east-1:000000000000:jobs"

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// recordingGate captures deliveries. When run is set it performs them;
// otherwise it swallows them, so tests need no target service.
type recordingGate struct {
	mu         sync.Mutex
	deliveries []chaos.Delivery
	run        bool
	fail       error
}

func (g *recordingGate) Deliver(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
	g.mu.Lock()
	g.deliveries = append(g.deliveries, d)
	g.mu.Unlock()
	if g.fail != nil {
		return g.fail
	}
	if g.run {
		return deliver(ctx)
	}
	return nil
}

func (g *recordingGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.deliveries)
}

func openStore(t *testing.T) store.Store {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { metadata.Close() })
	return metadata
}

func newTestService(t *testing.T) (*Service, *clock, *recordingGate) {
	t.Helper()
	return newTestServiceOn(t, openStore(t))
}

func newTestServiceOn(t *testing.T, metadata store.Store) (*Service, *clock, *recordingGate) {
	t.Helper()
	c := &clock{now: t0}
	gate := &recordingGate{}
	svc := NewService(Options{Metadata: metadata, Now: c.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	svc.SetFaults(gate)
	return svc, c, gate
}

func requireCode(t *testing.T, err error, code string) *apierror.Error {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
	return apiErr
}

func input(name, expression string) ScheduleInput {
	return ScheduleInput{
		Name:               name,
		ScheduleExpression: expression,
		FlexibleTimeWindow: FlexibleTimeWindow{Mode: WindowOff},
		Target:             Target{Arn: queueARN, RoleArn: "arn:aws:iam::000000000000:role/scheduler", Input: `{"job":"<aws.scheduler.schedule-arn>"}`},
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestCreateScheduleAppliesDefaultsAndPlansTheFirstRun(t *testing.T) {
	svc, _, _ := newTestService(t)

	sch, err := svc.CreateSchedule(input("nightly", "rate(1 hour)"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sch.GroupName != DefaultGroup || sch.State != StateEnabled || sch.ActionAfterCompletion != ActionNone || sch.ScheduleExpressionTimezone != "UTC" {
		t.Fatalf("defaults not applied: %+v", sch)
	}
	if sch.Arn != "arn:aws:scheduler:us-east-1:000000000000:schedule/default/nightly" {
		t.Fatalf("unexpected arn %s", sch.Arn)
	}
	if sch.Target.RetryPolicy == nil || sch.Target.RetryPolicy.MaximumRetryAttempts != 185 || sch.Target.RetryPolicy.MaximumEventAgeInSeconds != 86400 {
		t.Fatalf("AWS fills in the default retry policy: %+v", sch.Target.RetryPolicy)
	}
	if sch.NextInvocation == nil || !sch.NextInvocation.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a rate schedule first runs one interval after creation: %v", sch.NextInvocation)
	}
	if !sch.CreationDate.Equal(t0) || !sch.LastModificationDate.Equal(t0) {
		t.Fatalf("timestamps: %+v", sch)
	}

	groups, _ := svc.ListGroups("")
	if len(groups) != 1 || groups[0].Name != DefaultGroup {
		t.Fatalf("the default group should exist implicitly: %+v", groups)
	}
	_, err = svc.CreateSchedule(input("nightly", "rate(1 hour)"))
	requireCode(t, err, "ConflictException")
}

func TestCreateScheduleValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	cases := map[string]func(*ScheduleInput){
		"bad name":            func(in *ScheduleInput) { in.Name = "has space" },
		"missing window mode": func(in *ScheduleInput) { in.FlexibleTimeWindow.Mode = "" },
		"bad state":           func(in *ScheduleInput) { in.State = "PAUSED" },
		"bad action":          func(in *ScheduleInput) { in.ActionAfterCompletion = "ARCHIVE" },
		"no target":           func(in *ScheduleInput) { in.Target.Arn = "" },
		"unsupported target":  func(in *ScheduleInput) { in.Target.Arn = "arn:aws:kinesis:us-east-1:000000000000:stream/x" },
		"bad expression":      func(in *ScheduleInput) { in.ScheduleExpression = "hourly" },
		"bad timezone":        func(in *ScheduleInput) { in.ScheduleExpressionTimezone = "Mars/Olympus" },
		"end before start": func(in *ScheduleInput) {
			in.StartDate, in.EndDate = ptr(t0.Add(time.Hour)), ptr(t0)
		},
		"dates on a one-time schedule": func(in *ScheduleInput) {
			in.ScheduleExpression, in.StartDate = "at(2026-12-01T00:00:00)", ptr(t0)
		},
	}
	for name, mutate := range cases {
		in := input("s", "rate(1 hour)")
		mutate(&in)
		if _, err := svc.CreateSchedule(in); err == nil {
			t.Errorf("%s: want ValidationException", name)
		} else {
			requireCode(t, err, "ValidationException")
		}
	}
	in := input("s", "rate(1 hour)")
	in.GroupName = "missing"
	requireCode(t, scheduleErr(svc.CreateSchedule(in)), "ResourceNotFoundException")
}

func scheduleErr(_ Schedule, err error) error { return err }

func TestStartAndEndDatesBoundTheRuns(t *testing.T) {
	svc, c, gate := newTestService(t)

	in := input("window", "rate(10 minutes)")
	in.StartDate = ptr(t0.Add(time.Hour))
	in.EndDate = ptr(t0.Add(time.Hour + 15*time.Minute))
	sch, err := svc.CreateSchedule(in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !sch.NextInvocation.Equal(t0.Add(time.Hour)) {
		t.Fatalf("with a start date the first run is the start date, got %v", sch.NextInvocation)
	}

	c.now = t0.Add(30 * time.Minute)
	if fired, _ := svc.Tick(context.Background()); len(fired) != 0 {
		t.Fatalf("nothing is due before the start date: %+v", fired)
	}
	c.now = t0.Add(time.Hour)
	fired, _ := svc.Tick(context.Background())
	if len(fired) != 1 || !fired[0].ScheduledTime.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the start-date run: %+v", fired)
	}
	sch, _ = svc.GetSchedule("", "window")
	if !sch.NextInvocation.Equal(t0.Add(time.Hour + 10*time.Minute)) {
		t.Fatalf("next run: %v", sch.NextInvocation)
	}
	c.now = t0.Add(time.Hour + 10*time.Minute)
	_, _ = svc.Tick(context.Background())
	sch, _ = svc.GetSchedule("", "window")
	if sch.NextInvocation != nil {
		t.Fatalf("the run after the end date must not be planned: %v", sch.NextInvocation)
	}
	if gate.count() != 2 {
		t.Fatalf("want 2 deliveries, got %d", gate.count())
	}
}

func TestAdvanceFiresDueSchedulesOnceAndPlansFromTheNewTime(t *testing.T) {
	svc, _, gate := newTestService(t)
	if _, err := svc.CreateSchedule(input("minutely", "rate(1 minute)")); err != nil {
		t.Fatal(err)
	}
	daily := input("daily", "cron(0 9 * * ? *)")
	if _, err := svc.CreateSchedule(daily); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Advance(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !result.Now.Equal(t0.Add(time.Hour)) || result.Offset != "1h0m0s" {
		t.Fatalf("clock: %+v", result)
	}
	if len(result.Fired) != 1 || result.Fired[0].Name != "minutely" || !result.Fired[0].ScheduledTime.Equal(t0.Add(time.Minute)) {
		t.Fatalf("one rate run fires per advance, at its planned time: %+v", result.Fired)
	}
	if !strings.Contains(result.Fired[0].Input, `"job":"arn:aws:scheduler:us-east-1:000000000000:schedule/default/minutely"`) {
		t.Fatalf("input placeholders: %s", result.Fired[0].Input)
	}
	sch, _ := svc.GetSchedule("", "minutely")
	if !sch.NextInvocation.Equal(t0.Add(time.Hour+time.Minute)) || !sch.LastInvocation.Equal(t0.Add(time.Minute)) {
		t.Fatalf("the next run is planned from the new time: next=%v last=%v", sch.NextInvocation, sch.LastInvocation)
	}

	result, _ = svc.Advance(context.Background(), 21*time.Hour)
	names := map[string]bool{}
	for _, inv := range result.Fired {
		names[inv.Name] = true
	}
	if len(result.Fired) != 2 || !names["daily"] || !names["minutely"] {
		t.Fatalf("both schedules are due by 10:00 the next day: %+v", result.Fired)
	}
	if gate.count() != 3 {
		t.Fatalf("deliveries: %d", gate.count())
	}
	d := gate.deliveries[0]
	if d.Source != "scheduler" || d.Service != "sqs" || d.Operation != "SendMessage" || d.TargetARN != queueARN || !d.Async || d.SourceARN != sch.Arn {
		t.Fatalf("delivery description: %+v", d)
	}

	_, err = svc.Advance(context.Background(), 0)
	requireCode(t, err, "ValidationException")
	_, err = svc.AdvanceTo(context.Background(), t0)
	requireCode(t, err, "ValidationException")
	if result, err := svc.AdvanceTo(context.Background(), t0.Add(48*time.Hour)); err != nil || !result.Now.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("advance to: %+v %v", result, err)
	}
}

func TestClockOffsetLivesInTheStore(t *testing.T) {
	metadata := openStore(t)
	svc, _, _ := newTestServiceOn(t, metadata)
	if _, err := svc.Advance(context.Background(), 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	again, _, _ := newTestServiceOn(t, metadata)
	if again.Offset() != 2*time.Hour || !again.Now().Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("a new service on the same store should see the offset: %s", again.Offset())
	}
	if err := metadata.DeletePrefix(clockBucket, ""); err != nil {
		t.Fatal(err)
	}
	if again.Offset() != 0 {
		t.Fatal("clearing the bucket (what reset does) returns the clock to real time")
	}
}

func TestOneTimeScheduleCompletesAndCanDeleteItself(t *testing.T) {
	svc, c, _ := newTestService(t)
	keep := input("keep", "at(2026-11-09T13:00:00)")
	remove := input("remove", "at(2026-11-09T13:00:00)")
	remove.ActionAfterCompletion = ActionDelete
	for _, in := range []ScheduleInput{keep, remove} {
		if _, err := svc.CreateSchedule(in); err != nil {
			t.Fatal(err)
		}
	}
	c.now = t0.Add(time.Hour)
	fired, _ := svc.Tick(context.Background())
	if len(fired) != 2 {
		t.Fatalf("both fire at 13:00: %+v", fired)
	}
	kept, err := svc.GetSchedule("", "keep")
	if err != nil || kept.NextInvocation != nil || kept.LastInvocation == nil {
		t.Fatalf("a completed schedule stays, with no next run: %+v %v", kept, err)
	}
	requireCode(t, scheduleErr(svc.GetSchedule("", "remove")), "ResourceNotFoundException")
	if fired, _ := svc.Tick(context.Background()); len(fired) != 0 {
		t.Fatalf("completed schedules do not fire again: %+v", fired)
	}
}

func TestDisabledSchedulesNeverFireAndUpdateReplans(t *testing.T) {
	svc, c, gate := newTestService(t)
	in := input("job", "rate(5 minutes)")
	in.State = StateDisabled
	if _, err := svc.CreateSchedule(in); err != nil {
		t.Fatal(err)
	}
	c.now = t0.Add(time.Hour)
	if fired, _ := svc.Tick(context.Background()); len(fired) != 0 || gate.count() != 0 {
		t.Fatalf("disabled schedules stay quiet: %+v", fired)
	}

	in.State = StateEnabled
	in.ScheduleExpression = "rate(30 minutes)"
	in.Description = "re-enabled"
	updated, err := svc.UpdateSchedule(in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.CreationDate.Equal(t0) || !updated.LastModificationDate.Equal(c.now) || !updated.NextInvocation.Equal(c.now.Add(30*time.Minute)) || updated.Description != "re-enabled" {
		t.Fatalf("update keeps the creation date and plans from now: %+v", updated)
	}
	in.Name = "unknown"
	requireCode(t, scheduleErr(svc.UpdateSchedule(in)), "ResourceNotFoundException")
}

func TestTickClaimsBeforeDeliveringSoRunsAreNeverDoubled(t *testing.T) {
	svc, c, gate := newTestService(t)
	if _, err := svc.CreateSchedule(input("job", "rate(1 minute)")); err != nil {
		t.Fatal(err)
	}
	c.now = t0.Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Tick(context.Background())
		}()
	}
	wg.Wait()
	if gate.count() != 1 {
		t.Fatalf("concurrent ticks must fire a due schedule exactly once, got %d", gate.count())
	}
}

func TestRunFiresNowRegardlessOfPlanAndReportsTargetErrors(t *testing.T) {
	svc, _, gate := newTestService(t)
	in := input("job", "cron(0 9 ? * MON-FRI *)")
	in.State = StateDisabled
	if _, err := svc.CreateSchedule(in); err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Run(context.Background(), "", "job")
	if err != nil || inv.Error != "" || !inv.ScheduledTime.Equal(t0) || inv.ExecutionID == "" {
		t.Fatalf("run: %+v %v", inv, err)
	}
	sch, _ := svc.GetSchedule("", "job")
	if sch.LastInvocation != nil {
		t.Fatal("a manual run does not touch the plan")
	}

	gate.fail = errors.New("ThrottlingException: injected by chaos rule r1")
	inv, _ = svc.Run(context.Background(), DefaultGroup, "job")
	if !strings.Contains(inv.Error, "injected") {
		t.Fatalf("delivery errors are reported, not hidden: %+v", inv)
	}
	_, err = svc.Run(context.Background(), "", "missing")
	requireCode(t, err, "ResourceNotFoundException")
}

func TestDeliversToARealSQSQueue(t *testing.T) {
	metadata := openStore(t)
	svc, _, gate := newTestServiceOn(t, metadata)
	gate.run = true
	queues := sqs.NewService(metadata)
	svc.SetSQS(queues)
	sqsCall(t, queues, "CreateQueue", url.Values{"QueueName": {"jobs"}})

	in := input("job", "rate(1 minute)")
	in.Target.Input = `{"at":"<aws.scheduler.scheduled-time>","id":"<aws.scheduler.execution-id>","attempt":<aws.scheduler.attempt-number>}`
	if _, err := svc.CreateSchedule(in); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Advance(context.Background(), time.Minute)
	if err != nil || len(result.Fired) != 1 || result.Fired[0].Error != "" {
		t.Fatalf("advance: %+v %v", result, err)
	}
	body := sqsCall(t, queues, "ReceiveMessage", url.Values{"QueueUrl": {"http://localhost:4566/000000000000/jobs"}, "MaxNumberOfMessages": {"10"}})
	if strings.Count(body, "<Message>") != 1 || !strings.Contains(body, "2026-11-09T12:01:00Z") || !strings.Contains(body, result.Fired[0].ExecutionID) || !strings.Contains(body, `&#34;attempt&#34;:1`) {
		t.Fatalf("the queue should hold the rendered input:\n%s", body)
	}

	// A missing queue is reported on the invocation rather than lost.
	bad := input("orphan", "rate(1 minute)")
	bad.Target.Arn = "arn:aws:sqs:us-east-1:000000000000:nowhere"
	if _, err := svc.CreateSchedule(bad); err != nil {
		t.Fatal(err)
	}
	inv, _ := svc.Run(context.Background(), "", "orphan")
	if inv.Error == "" {
		t.Fatal("sending to a missing queue must surface an error")
	}
}

func sqsCall(t *testing.T, svc *sqs.Service, action string, form url.Values) string {
	t.Helper()
	form.Set("Action", action)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, req, action, "req"); err != nil {
		t.Fatalf("sqs %s: %v", action, err)
	}
	return rec.Body.String()
}

func TestGroupsOwnTheirSchedules(t *testing.T) {
	svc, _, _ := newTestService(t)
	g, err := svc.CreateGroup("billing", map[string]string{"team": "payments"})
	if err != nil || g.Arn != "arn:aws:scheduler:us-east-1:000000000000:schedule-group/billing" || g.State != "ACTIVE" {
		t.Fatalf("create group: %+v %v", g, err)
	}
	requireCode(t, groupErr(svc.CreateGroup("billing", nil)), "ConflictException")
	requireCode(t, groupErr(svc.CreateGroup("bad name", nil)), "ValidationException")

	in := input("invoice", "rate(1 day)")
	in.GroupName = "billing"
	if _, err := svc.CreateSchedule(in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSchedule(input("invoice", "rate(1 day)")); err != nil {
		t.Fatalf("the same name in another group is fine: %v", err)
	}
	if _, err := svc.CreateSchedule(input("other", "rate(1 day)")); err != nil {
		t.Fatal(err)
	}

	all, _ := svc.ListSchedules(ListFilter{})
	billing, _ := svc.ListSchedules(ListFilter{GroupName: "billing"})
	byPrefix, _ := svc.ListSchedules(ListFilter{NamePrefix: "inv"})
	disabled, _ := svc.ListSchedules(ListFilter{State: StateDisabled})
	if len(all) != 3 || len(billing) != 1 || len(byPrefix) != 2 || len(disabled) != 0 {
		t.Fatalf("filters: all=%d billing=%d prefix=%d disabled=%d", len(all), len(billing), len(byPrefix), len(disabled))
	}
	if all[0].GroupName != "billing" || all[1].Name != "invoice" || all[2].Name != "other" {
		t.Fatalf("listing is sorted by group then name: %+v", all)
	}

	if err := svc.TagGroup("billing", map[string]string{"env": "test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UntagGroup("billing", []string{"team"}); err != nil {
		t.Fatal(err)
	}
	g, _ = svc.GetGroup("billing")
	if len(g.Tags) != 1 || g.Tags["env"] != "test" {
		t.Fatalf("tags: %+v", g.Tags)
	}

	requireCode(t, svc.DeleteGroup(DefaultGroup), "ValidationException")
	requireCode(t, svc.DeleteGroup("missing"), "ResourceNotFoundException")
	if err := svc.DeleteGroup("billing"); err != nil {
		t.Fatal(err)
	}
	requireCode(t, scheduleErr(svc.GetSchedule("billing", "invoice")), "ResourceNotFoundException")
	if rest, _ := svc.ListSchedules(ListFilter{}); len(rest) != 2 {
		t.Fatalf("deleting a group removes only its schedules: %+v", rest)
	}
	if err := svc.DeleteSchedule("", "other"); err != nil {
		t.Fatal(err)
	}
	requireCode(t, svc.DeleteSchedule("", "other"), "ResourceNotFoundException")
}

func groupErr(_ Group, err error) error { return err }

func TestStartAndCloseRunTheTicker(t *testing.T) {
	svc, c, gate := newTestService(t)
	if _, err := svc.CreateSchedule(input("job", "rate(1 minute)")); err != nil {
		t.Fatal(err)
	}
	c.now = t0.Add(time.Minute)
	svc.Start()
	deadline := time.Now().Add(5 * time.Second)
	for gate.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if gate.count() != 1 {
		t.Fatalf("the ticker should have fired the due schedule once, got %d", gate.count())
	}
	if err := svc.Close(); err != nil {
		t.Fatal("closing twice is harmless")
	}
	idle := NewService(Options{Metadata: openStore(t)})
	if err := idle.Close(); err != nil {
		t.Fatal("closing a never-started service is harmless")
	}
}
