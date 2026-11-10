package scheduler

import (
	"github.com/stratus/internal/scope"

	"regexp"
	"strings"
	"time"
)

const (
	// DefaultGroup is the schedule group AWS creates for every account.
	DefaultGroup = "default"

	StateEnabled  = "ENABLED"
	StateDisabled = "DISABLED"

	ActionNone   = "NONE"
	ActionDelete = "DELETE"

	WindowOff      = "OFF"
	WindowFlexible = "FLEXIBLE"
)

var nameRe = regexp.MustCompile(`^[0-9a-zA-Z\-_.]{1,64}$`)

// Target is where a schedule delivers. Stratus delivers to Lambda functions,
// SQS queues, SNS topics and Step Functions state machines; the other
// parameter blocks are kept so GetSchedule echoes what was configured.
type Target struct {
	Arn                   string                 `json:"Arn"`
	RoleArn               string                 `json:"RoleArn,omitempty"`
	Input                 string                 `json:"Input,omitempty"`
	DeadLetterConfig      *DeadLetterConfig      `json:"DeadLetterConfig,omitempty"`
	RetryPolicy           *RetryPolicy           `json:"RetryPolicy,omitempty"`
	SqsParameters         *SqsParameters         `json:"SqsParameters,omitempty"`
	EventBridgeParameters *EventBridgeParameters `json:"EventBridgeParameters,omitempty"`
	KinesisParameters     *KinesisParameters     `json:"KinesisParameters,omitempty"`
}

type DeadLetterConfig struct {
	Arn string `json:"Arn,omitempty"`
}

type RetryPolicy struct {
	MaximumEventAgeInSeconds int `json:"MaximumEventAgeInSeconds,omitempty"`
	MaximumRetryAttempts     int `json:"MaximumRetryAttempts"`
}

type SqsParameters struct {
	MessageGroupId string `json:"MessageGroupId,omitempty"`
}

type EventBridgeParameters struct {
	DetailType string `json:"DetailType,omitempty"`
	Source     string `json:"Source,omitempty"`
}

type KinesisParameters struct {
	PartitionKey string `json:"PartitionKey,omitempty"`
}

// FlexibleTimeWindow is accepted for API compatibility. Stratus always fires
// at the scheduled time so tests stay deterministic.
type FlexibleTimeWindow struct {
	Mode                   string `json:"Mode"`
	MaximumWindowInMinutes int    `json:"MaximumWindowInMinutes,omitempty"`
}

// ScheduleInput is what CreateSchedule and UpdateSchedule take.
type ScheduleInput struct {
	Name                       string
	GroupName                  string
	Description                string
	State                      string
	ScheduleExpression         string
	ScheduleExpressionTimezone string
	StartDate                  *time.Time
	EndDate                    *time.Time
	FlexibleTimeWindow         FlexibleTimeWindow
	ActionAfterCompletion      string
	KmsKeyArn                  string
	Target                     Target
}

// Schedule is a stored schedule together with its invocation plan.
type Schedule struct {
	Name                       string             `json:"name"`
	GroupName                  string             `json:"group_name"`
	Arn                        string             `json:"arn"`
	Description                string             `json:"description,omitempty"`
	State                      string             `json:"state"`
	ScheduleExpression         string             `json:"schedule_expression"`
	ScheduleExpressionTimezone string             `json:"schedule_expression_timezone"`
	StartDate                  *time.Time         `json:"start_date,omitempty"`
	EndDate                    *time.Time         `json:"end_date,omitempty"`
	FlexibleTimeWindow         FlexibleTimeWindow `json:"flexible_time_window"`
	ActionAfterCompletion      string             `json:"action_after_completion"`
	KmsKeyArn                  string             `json:"kms_key_arn,omitempty"`
	Target                     Target             `json:"target"`
	CreationDate               time.Time          `json:"creation_date"`
	LastModificationDate       time.Time          `json:"last_modification_date"`
	// NextInvocation is when the schedule fires next on the service clock,
	// or nil once a schedule has completed.
	NextInvocation *time.Time `json:"next_invocation,omitempty"`
	// LastInvocation is the scheduled time of the most recent firing.
	LastInvocation *time.Time `json:"last_invocation,omitempty"`
}

// Group is a schedule group.
type Group struct {
	Name                 string            `json:"name"`
	Arn                  string            `json:"arn"`
	State                string            `json:"state"`
	Tags                 map[string]string `json:"tags,omitempty"`
	CreationDate         time.Time         `json:"creation_date"`
	LastModificationDate time.Time         `json:"last_modification_date"`
}

// Invocation records one delivery of a schedule to its target.
type Invocation struct {
	Name          string    `json:"name"`
	Group         string    `json:"group"`
	ScheduleArn   string    `json:"schedule_arn"`
	TargetArn     string    `json:"target"`
	ScheduledTime time.Time `json:"scheduled_time"`
	ExecutionID   string    `json:"execution_id"`
	Input         string    `json:"input"`
	// Error is set when the target refused the delivery, for example because
	// the queue does not exist or a chaos rule failed it.
	Error string `json:"error,omitempty"`
}

// ListFilter narrows ListSchedules.
type ListFilter struct {
	GroupName  string
	NamePrefix string
	State      string
}

func scheduleARN(sc scope.Scope, group, name string) string {
	return sc.ARN("scheduler", "schedule/"+group+"/"+name)
}

func groupARN(sc scope.Scope, name string) string {
	return sc.ARN("scheduler", "schedule-group/"+name)
}

// targetKind names the service behind a target ARN, or "" when Stratus
// cannot deliver to it.
func targetKind(arn string) string {
	switch {
	case strings.Contains(arn, ":lambda:") && strings.Contains(arn, ":function:"):
		return "lambda"
	case strings.Contains(arn, ":sqs:"):
		return "sqs"
	case strings.Contains(arn, ":sns:"):
		return "sns"
	case strings.Contains(arn, ":states:") && strings.Contains(arn, ":stateMachine:"):
		return "stepfunctions"
	default:
		return ""
	}
}

func lambdaNameFromARN(arn string) string {
	_, name, ok := strings.Cut(arn, ":function:")
	if !ok {
		return arn
	}
	// Strip a qualifier such as :prod, if any.
	name, _, _ = strings.Cut(name, ":")
	return name
}

// renderInput substitutes the context attributes AWS supports in a target's
// input, and defaults an empty input to an empty JSON object.
func renderInput(input string, inv Invocation) string {
	if input == "" {
		input = "{}"
	}
	replacer := strings.NewReplacer(
		"<aws.scheduler.schedule-arn>", inv.ScheduleArn,
		"<aws.scheduler.scheduled-time>", inv.ScheduledTime.UTC().Format("2006-01-02T15:04:05Z"),
		"<aws.scheduler.execution-id>", inv.ExecutionID,
		"<aws.scheduler.attempt-number>", "1",
	)
	return replacer.Replace(input)
}
