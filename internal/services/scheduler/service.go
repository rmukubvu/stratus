package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/sns"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/stepfunctions"
	"github.com/stratus/internal/store"
)

const (
	schedulesBucket = "scheduler-schedules"
	groupsBucket    = "scheduler-groups"
	clockBucket     = "scheduler-clock"
	clockKey        = "offset"

	tickInterval = 250 * time.Millisecond
)

// Options configures the scheduler.
type Options struct {
	Metadata store.Store
	// Now is the real clock; the service clock adds the fast-forward offset.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service emulates EventBridge Scheduler. Its clock can be advanced through
// the control API so tests never wait for wall-clock time; the offset lives
// in the metadata store, so reset and snapshots cover it.
type Service struct {
	metadata      store.Store
	realNow       func() time.Time
	logger        *slog.Logger
	faults        chaos.Gate
	lambda        *lambdasvc.Service
	sqs           *sqs.Service
	sns           *sns.Service
	stepFunctions *stepfunctions.Service

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
	stop     chan struct{}
	stopped  chan struct{}
}

// NewService builds the service. Call Start to run the background ticker.
func NewService(opts Options) *Service {
	svc := &Service{
		metadata: opts.Metadata,
		realNow:  opts.Now,
		logger:   opts.Logger,
		faults:   chaos.Passthrough{},
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	if svc.realNow == nil {
		svc.realNow = time.Now
	}
	if svc.logger == nil {
		svc.logger = slog.Default()
	}
	return svc
}

// SetFaults routes deliveries through a chaos gate.
func (s *Service) SetFaults(gate chaos.Gate) { s.faults = gate }

func (s *Service) SetLambda(target *lambdasvc.Service)            { s.lambda = target }
func (s *Service) SetSQS(target *sqs.Service)                     { s.sqs = target }
func (s *Service) SetSNS(target *sns.Service)                     { s.sns = target }
func (s *Service) SetStepFunctions(target *stepfunctions.Service) { s.stepFunctions = target }

// Start runs the ticker that fires due schedules in the background.
func (s *Service) Start() {
	s.started = true
	go func() {
		defer close(s.stopped)
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				if _, err := s.Tick(context.Background()); err != nil {
					s.logger.Warn("scheduler tick failed", "error", err)
				}
			}
		}
	}()
}

// Close stops the ticker started by Start and waits for it to finish. It
// is safe without Start, and safe to call twice.
func (s *Service) Close() error {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.started {
			<-s.stopped
		}
	})
	return nil
}

// Clock

type clockRecord struct {
	OffsetNanos int64 `json:"offset_ns"`
}

// Offset is how far the service clock runs ahead of the real one.
func (s *Service) Offset() time.Duration {
	raw, err := s.metadata.Get(clockBucket, clockKey)
	if err != nil || raw == nil {
		return 0
	}
	var rec clockRecord
	if json.Unmarshal(raw, &rec) != nil {
		return 0
	}
	return time.Duration(rec.OffsetNanos)
}

// Now is the service clock: real time plus the fast-forward offset.
func (s *Service) Now() time.Time {
	return s.realNow().Add(s.Offset()).UTC()
}

// AdvanceResult reports a clock change and what it fired.
type AdvanceResult struct {
	Now    time.Time    `json:"now"`
	Offset string       `json:"offset"`
	Fired  []Invocation `json:"fired"`
}

// Advance moves the service clock forward by d and fires every schedule that
// became due, once each, synchronously.
func (s *Service) Advance(ctx context.Context, d time.Duration) (AdvanceResult, error) {
	if d <= 0 {
		return AdvanceResult{}, validation("the clock only moves forward; by must be a positive duration such as 30m")
	}
	s.mu.Lock()
	err := s.saveOffset(s.Offset() + d)
	s.mu.Unlock()
	if err != nil {
		return AdvanceResult{}, err
	}
	fired, err := s.Tick(ctx)
	if err != nil {
		return AdvanceResult{}, err
	}
	return s.advanceResult(fired), nil
}

// AdvanceTo moves the service clock to the instant to, which must be ahead
// of the current service time.
func (s *Service) AdvanceTo(ctx context.Context, to time.Time) (AdvanceResult, error) {
	d := to.Sub(s.Now())
	if d <= 0 {
		return AdvanceResult{}, validation("to must be later than the current scheduler time %s", s.Now().Format(time.RFC3339))
	}
	return s.Advance(ctx, d)
}

func (s *Service) advanceResult(fired []Invocation) AdvanceResult {
	return AdvanceResult{Now: s.Now(), Offset: s.Offset().String(), Fired: fired}
}

func (s *Service) saveOffset(d time.Duration) error {
	raw, _ := json.Marshal(clockRecord{OffsetNanos: int64(d)})
	if err := s.metadata.Put(clockBucket, clockKey, raw); err != nil {
		return internal(err)
	}
	return nil
}

// Schedules

// CreateSchedule validates, plans and stores a schedule.
func (s *Service) CreateSchedule(in ScheduleInput) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return Schedule{}, err
	}
	now := s.Now()
	sch, err := s.build(in, now, now)
	if err != nil {
		return Schedule{}, err
	}
	existing, err := s.load(sch.GroupName, sch.Name)
	if err != nil {
		return Schedule{}, err
	}
	if existing != nil {
		return Schedule{}, conflict("schedule %s already exists in group %s", sch.Name, sch.GroupName)
	}
	if err := s.save(sch); err != nil {
		return Schedule{}, err
	}
	return sch, nil
}

// UpdateSchedule replaces a schedule's definition and plans it afresh.
func (s *Service) UpdateSchedule(in ScheduleInput) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return Schedule{}, err
	}
	existing, err := s.load(groupOrDefault(in.GroupName), in.Name)
	if err != nil {
		return Schedule{}, err
	}
	if existing == nil {
		return Schedule{}, scheduleNotFound(groupOrDefault(in.GroupName), in.Name)
	}
	sch, err := s.build(in, existing.CreationDate, s.Now())
	if err != nil {
		return Schedule{}, err
	}
	sch.LastInvocation = existing.LastInvocation
	if err := s.save(sch); err != nil {
		return Schedule{}, err
	}
	return sch, nil
}

// GetSchedule returns one schedule.
func (s *Service) GetSchedule(group, name string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	group = groupOrDefault(group)
	sch, err := s.load(group, name)
	if err != nil {
		return Schedule{}, err
	}
	if sch == nil {
		return Schedule{}, scheduleNotFound(group, name)
	}
	return *sch, nil
}

// DeleteSchedule removes a schedule.
func (s *Service) DeleteSchedule(group, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	group = groupOrDefault(group)
	sch, err := s.load(group, name)
	if err != nil {
		return err
	}
	if sch == nil {
		return scheduleNotFound(group, name)
	}
	if err := s.metadata.Delete(schedulesBucket, scheduleKey(group, name)); err != nil {
		return internal(err)
	}
	return nil
}

// ListSchedules returns schedules matching filter, sorted by group then name.
func (s *Service) ListSchedules(filter ListFilter) ([]Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.scanSchedules()
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, 0, len(all))
	for _, sch := range all {
		if filter.GroupName != "" && sch.GroupName != filter.GroupName {
			continue
		}
		if filter.NamePrefix != "" && !strings.HasPrefix(sch.Name, filter.NamePrefix) {
			continue
		}
		if filter.State != "" && sch.State != filter.State {
			continue
		}
		out = append(out, sch)
	}
	return out, nil
}

func (s *Service) build(in ScheduleInput, createdAt, now time.Time) (Schedule, error) {
	if !nameRe.MatchString(in.Name) {
		return Schedule{}, validation("schedule name must be 1-64 characters of letters, digits, '-', '_' or '.', got %q", in.Name)
	}
	group := groupOrDefault(in.GroupName)
	if g, err := s.loadGroup(group); err != nil {
		return Schedule{}, err
	} else if g == nil {
		return Schedule{}, groupNotFound(group)
	}
	state := in.State
	if state == "" {
		state = StateEnabled
	}
	if state != StateEnabled && state != StateDisabled {
		return Schedule{}, validation("State must be ENABLED or DISABLED, got %q", in.State)
	}
	action := in.ActionAfterCompletion
	if action == "" {
		action = ActionNone
	}
	if action != ActionNone && action != ActionDelete {
		return Schedule{}, validation("ActionAfterCompletion must be NONE or DELETE, got %q", in.ActionAfterCompletion)
	}
	if in.FlexibleTimeWindow.Mode != WindowOff && in.FlexibleTimeWindow.Mode != WindowFlexible {
		return Schedule{}, validation("FlexibleTimeWindow.Mode is required and must be OFF or FLEXIBLE")
	}
	if in.Target.Arn == "" {
		return Schedule{}, validation("Target.Arn is required")
	}
	if targetKind(in.Target.Arn) == "" {
		return Schedule{}, validation("Stratus delivers schedules to Lambda functions, SQS queues, SNS topics and Step Functions state machines; %q is none of those", in.Target.Arn)
	}
	if in.StartDate != nil && in.EndDate != nil && !in.EndDate.After(*in.StartDate) {
		return Schedule{}, validation("EndDate must be later than StartDate")
	}
	tz := in.ScheduleExpressionTimezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Schedule{}, validation("ScheduleExpressionTimezone %q is not an IANA time zone", tz)
	}
	sch := Schedule{
		Name:                       in.Name,
		GroupName:                  group,
		Arn:                        scheduleARN(group, in.Name),
		Description:                in.Description,
		State:                      state,
		ScheduleExpression:         strings.TrimSpace(in.ScheduleExpression),
		ScheduleExpressionTimezone: tz,
		StartDate:                  in.StartDate,
		EndDate:                    in.EndDate,
		FlexibleTimeWindow:         in.FlexibleTimeWindow,
		ActionAfterCompletion:      action,
		KmsKeyArn:                  in.KmsKeyArn,
		Target:                     withDefaultRetry(in.Target),
		CreationDate:               createdAt,
		LastModificationDate:       now,
	}
	expr, err := s.compile(sch, loc)
	if err != nil {
		return Schedule{}, err
	}
	if !expr.Recurring() && (sch.StartDate != nil || sch.EndDate != nil) {
		return Schedule{}, validation("StartDate and EndDate cannot be used with a one-time at() schedule")
	}
	sch.NextInvocation = plan(expr, sch, now)
	return sch, nil
}

func (s *Service) compile(sch Schedule, loc *time.Location) (Expression, error) {
	anchor := sch.CreationDate
	if sch.StartDate != nil {
		anchor = *sch.StartDate
	}
	expr, err := ParseExpression(sch.ScheduleExpression, loc, anchor)
	if err != nil {
		return nil, validation("invalid ScheduleExpression: %s", err.Error())
	}
	return expr, nil
}

// plan returns the next invocation strictly after now, honouring the start
// and end dates, or nil when the schedule is complete.
func plan(expr Expression, sch Schedule, now time.Time) *time.Time {
	from := now
	if sch.StartDate != nil && sch.StartDate.After(from) {
		from = sch.StartDate.Add(-time.Nanosecond)
	}
	next, ok := expr.Next(from)
	if !ok || (sch.EndDate != nil && next.After(*sch.EndDate)) {
		return nil
	}
	next = next.UTC()
	return &next
}

func withDefaultRetry(t Target) Target {
	if t.RetryPolicy == nil {
		t.RetryPolicy = &RetryPolicy{MaximumEventAgeInSeconds: 86400, MaximumRetryAttempts: 185}
	}
	return t
}

// Groups

// CreateGroup creates a schedule group.
func (s *Service) CreateGroup(name string, tags map[string]string) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return Group{}, err
	}
	if !nameRe.MatchString(name) {
		return Group{}, validation("schedule group name must be 1-64 characters of letters, digits, '-', '_' or '.', got %q", name)
	}
	existing, err := s.loadGroup(name)
	if err != nil {
		return Group{}, err
	}
	if existing != nil {
		return Group{}, conflict("schedule group %s already exists", name)
	}
	return s.saveGroup(name, tags)
}

// GetGroup returns one schedule group.
func (s *Service) GetGroup(name string) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return Group{}, err
	}
	g, err := s.loadGroup(name)
	if err != nil {
		return Group{}, err
	}
	if g == nil {
		return Group{}, groupNotFound(name)
	}
	return *g, nil
}

// DeleteGroup removes a group and every schedule in it.
func (s *Service) DeleteGroup(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == DefaultGroup {
		return validation("the default schedule group cannot be deleted")
	}
	g, err := s.loadGroup(name)
	if err != nil {
		return err
	}
	if g == nil {
		return groupNotFound(name)
	}
	if err := s.metadata.DeletePrefix(schedulesBucket, name+"/"); err != nil {
		return internal(err)
	}
	if err := s.metadata.Delete(groupsBucket, name); err != nil {
		return internal(err)
	}
	return nil
}

// ListGroups returns the schedule groups whose name starts with prefix.
func (s *Service) ListGroups(prefix string) ([]Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return nil, err
	}
	var out []Group
	err := s.metadata.Scan(groupsBucket, prefix, func(_, v []byte) error {
		var g Group
		if json.Unmarshal(v, &g) == nil {
			out = append(out, g)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TagGroup adds tags to a group; UntagGroup removes them.
func (s *Service) TagGroup(name string, tags map[string]string) error {
	return s.updateGroupTags(name, func(current map[string]string) {
		for k, v := range tags {
			current[k] = v
		}
	})
}

func (s *Service) UntagGroup(name string, keys []string) error {
	return s.updateGroupTags(name, func(current map[string]string) {
		for _, k := range keys {
			delete(current, k)
		}
	})
}

func (s *Service) updateGroupTags(name string, apply func(map[string]string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaultGroup(); err != nil {
		return err
	}
	g, err := s.loadGroup(name)
	if err != nil {
		return err
	}
	if g == nil {
		return groupNotFound(name)
	}
	if g.Tags == nil {
		g.Tags = map[string]string{}
	}
	apply(g.Tags)
	g.LastModificationDate = s.Now()
	return s.putGroup(*g)
}

// Firing

// Tick fires every enabled schedule whose next invocation is due on the
// service clock. Each schedule fires at most once per tick; its next
// invocation is then planned from the current time, so advancing the clock
// by an hour over a one-minute rate yields one delivery, not sixty.
func (s *Service) Tick(ctx context.Context) ([]Invocation, error) {
	due, err := s.claimDue()
	if err != nil {
		return nil, err
	}
	invocations := make([]Invocation, 0, len(due))
	for _, d := range due {
		invocations = append(invocations, s.deliver(ctx, d.schedule, d.at))
	}
	return invocations, nil
}

type dueSchedule struct {
	schedule Schedule
	at       time.Time
}

// claimDue persists the plan for every due schedule before anything is
// delivered, so a concurrent tick never fires the same invocation twice.
func (s *Service) claimDue() ([]dueSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.scanSchedules()
	if err != nil {
		return nil, err
	}
	now := s.Now()
	var due []dueSchedule
	for _, sch := range all {
		if sch.State != StateEnabled || sch.NextInvocation == nil || sch.NextInvocation.After(now) {
			continue
		}
		at := *sch.NextInvocation
		due = append(due, dueSchedule{schedule: sch, at: at})
		if err := s.replan(sch, at, now); err != nil {
			return nil, err
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].schedule.Arn < due[j].schedule.Arn
	})
	return due, nil
}

func (s *Service) replan(sch Schedule, fired, now time.Time) error {
	loc, err := time.LoadLocation(sch.ScheduleExpressionTimezone)
	if err != nil {
		loc = time.UTC
	}
	expr, err := s.compile(sch, loc)
	if err != nil {
		return err
	}
	sch.LastInvocation = &fired
	sch.NextInvocation = plan(expr, sch, now)
	if sch.NextInvocation == nil && sch.ActionAfterCompletion == ActionDelete {
		if err := s.metadata.Delete(schedulesBucket, scheduleKey(sch.GroupName, sch.Name)); err != nil {
			return internal(err)
		}
		return nil
	}
	return s.save(sch)
}

// Run delivers a schedule to its target right now, regardless of its state
// or plan. It is how a test triggers a schedule without touching the clock.
func (s *Service) Run(ctx context.Context, group, name string) (Invocation, error) {
	sch, err := s.GetSchedule(group, name)
	if err != nil {
		return Invocation{}, err
	}
	return s.deliver(ctx, sch, s.Now()), nil
}

func (s *Service) deliver(ctx context.Context, sch Schedule, scheduledTime time.Time) Invocation {
	inv := Invocation{
		Name:          sch.Name,
		Group:         sch.GroupName,
		ScheduleArn:   sch.Arn,
		TargetArn:     sch.Target.Arn,
		ScheduledTime: scheduledTime.UTC(),
		ExecutionID:   uuid.NewString(),
	}
	inv.Input = renderInput(sch.Target.Input, inv)
	delivery := chaos.Delivery{ID: inv.ExecutionID, Source: "scheduler", SourceARN: sch.Arn, TargetARN: sch.Target.Arn, Async: true}
	switch targetKind(sch.Target.Arn) {
	case "lambda":
		delivery.Service, delivery.Operation, delivery.Function = "lambda", "Invoke", lambdaNameFromARN(sch.Target.Arn)
	case "sqs":
		delivery.Service, delivery.Operation = "sqs", "SendMessage"
	case "sns":
		delivery.Service, delivery.Operation = "sns", "Publish"
	case "stepfunctions":
		delivery.Service, delivery.Operation = "stepfunctions", "StartExecution"
	}
	err := s.faults.Deliver(ctx, delivery, func(ctx context.Context) error {
		return s.send(ctx, sch.Target, inv.Input)
	})
	if err != nil {
		inv.Error = err.Error()
		s.logger.Warn("schedule delivery failed", "schedule", sch.Arn, "target", sch.Target.Arn, "error", err)
	}
	return inv
}

func (s *Service) send(ctx context.Context, target Target, input string) error {
	switch targetKind(target.Arn) {
	case "lambda":
		if s.lambda == nil {
			return fmt.Errorf("lambda is not available to the scheduler")
		}
		result, err := s.lambda.InvokeByName(ctx, lambdaNameFromARN(target.Arn), []byte(input))
		if err != nil {
			return err
		}
		if result.FunctionError != "" {
			return fmt.Errorf("function returned %s: %s", result.FunctionError, strings.TrimSpace(string(result.Payload)))
		}
		return nil
	case "sqs":
		if s.sqs == nil {
			return fmt.Errorf("sqs is not available to the scheduler")
		}
		return s.sqs.SendMessageToARN(target.Arn, input)
	case "sns":
		if s.sns == nil {
			return fmt.Errorf("sns is not available to the scheduler")
		}
		return s.sns.PublishToTopic(target.Arn, input)
	case "stepfunctions":
		if s.stepFunctions == nil {
			return fmt.Errorf("step functions is not available to the scheduler")
		}
		return s.stepFunctions.StartExecutionByARN(ctx, target.Arn, input)
	default:
		return fmt.Errorf("unsupported target %s", target.Arn)
	}
}

// Storage

func scheduleKey(group, name string) string { return group + "/" + name }

func groupOrDefault(group string) string {
	if group == "" {
		return DefaultGroup
	}
	return group
}

func (s *Service) load(group, name string) (*Schedule, error) {
	raw, err := s.metadata.Get(schedulesBucket, scheduleKey(group, name))
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return nil, nil
	}
	var sch Schedule
	if err := json.Unmarshal(raw, &sch); err != nil {
		return nil, internal(err)
	}
	return &sch, nil
}

func (s *Service) save(sch Schedule) error {
	raw, err := json.Marshal(sch)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(schedulesBucket, scheduleKey(sch.GroupName, sch.Name), raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) scanSchedules() ([]Schedule, error) {
	var out []Schedule
	err := s.metadata.Scan(schedulesBucket, "", func(_, v []byte) error {
		var sch Schedule
		if json.Unmarshal(v, &sch) == nil {
			out = append(out, sch)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupName != out[j].GroupName {
			return out[i].GroupName < out[j].GroupName
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Service) ensureDefaultGroup() error {
	g, err := s.loadGroup(DefaultGroup)
	if err != nil || g != nil {
		return err
	}
	_, err = s.saveGroup(DefaultGroup, nil)
	return err
}

func (s *Service) loadGroup(name string) (*Group, error) {
	raw, err := s.metadata.Get(groupsBucket, name)
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return nil, nil
	}
	var g Group
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, internal(err)
	}
	return &g, nil
}

func (s *Service) saveGroup(name string, tags map[string]string) (Group, error) {
	now := s.Now()
	g := Group{Name: name, Arn: groupARN(name), State: "ACTIVE", Tags: tags, CreationDate: now, LastModificationDate: now}
	return g, s.putGroup(g)
}

func (s *Service) putGroup(g Group) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(groupsBucket, g.Name, raw); err != nil {
		return internal(err)
	}
	return nil
}

// Errors, in the REST JSON vocabulary EventBridge Scheduler uses.

func validation(format string, args ...any) *apierror.Error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) *apierror.Error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "ConflictException", Message: fmt.Sprintf(format, args...)}
}

func scheduleNotFound(group, name string) *apierror.Error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: fmt.Sprintf("schedule %s not found in group %s", name, group)}
}

func groupNotFound(name string) *apierror.Error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: fmt.Sprintf("schedule group %s not found", name)}
}

func internal(err error) *apierror.Error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerException", Message: err.Error()}
}
