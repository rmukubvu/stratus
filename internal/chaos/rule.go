// Package chaos injects deterministic faults into AWS API traffic and into
// the event deliveries Stratus makes internally (SQS and stream event source
// mappings, SNS fan-out, EventBridge targets, Step Functions tasks and Lambda
// destinations).
//
// An Experiment is a set of Rules plus a seed. Each rule targets traffic by
// scope, service, operation and resource, and fires with a probability.
// Whether the nth item of traffic matching a rule is faulted depends only on
// (seed, rule, n), so replaying the same sequence with the same seed injects
// exactly the same faults. That makes a failing chaos run reproducible in CI
// and debuggable by a coding agent.
package chaos

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stratus/internal/control"
)

// Action is what a rule does to a matching request.
type Action string

const (
	// ActionError returns a custom AWS error (see ErrorSpec).
	ActionError Action = "error"
	// ActionThrottle returns the service's throttling error.
	ActionThrottle Action = "throttle"
	// ActionInternalError returns a 500 internal error.
	ActionInternalError Action = "internal_error"
	// ActionUnavailable returns a 503 service unavailable error.
	ActionUnavailable Action = "unavailable"
	// ActionLatency delays the request, then lets it through.
	ActionLatency Action = "latency"
	// ActionDrop closes the connection without a response.
	ActionDrop Action = "drop"
	// ActionColdStart discards the target function's warm container, so
	// the Lambda invocation pays a cold start.
	ActionColdStart Action = "cold_start"
	// ActionFunctionError makes a Lambda invocation fail the way a function
	// that throws does: Invoke returns 200 with X-Amz-Function-Error.
	ActionFunctionError Action = "function_error"
	// ActionDuplicate delivers an event twice, as at-least-once delivery
	// can. It only applies to event deliveries.
	ActionDuplicate Action = "duplicate"
)

// Scope says which kind of traffic a rule sees.
type Scope string

const (
	// ScopeAPI matches AWS API requests made to Stratus. It is the default.
	ScopeAPI Scope = "api"
	// ScopeEvent matches deliveries Stratus makes internally, from a source
	// service to a target, without an API request.
	ScopeEvent Scope = "event"
	// ScopeAll matches both.
	ScopeAll Scope = "all"
)

// AnyService matches every service.
const AnyService = "*"

// ErrorSpec is the AWS error a rule returns.
type ErrorSpec struct {
	StatusCode int    `json:"status_code"`
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
}

// LatencySpec is the delay a rule adds. The jitter is deterministic.
type LatencySpec struct {
	DelayMS  int `json:"delay_ms"`
	JitterMS int `json:"jitter_ms,omitempty"`
}

// Rule targets a slice of traffic and says what to do with it.
type Rule struct {
	ID string `json:"id"`
	// Scope is "api" (default), "event" or "all". It defaults to "event"
	// when Source is set.
	Scope Scope `json:"scope,omitempty"`
	// Service is the AWS service name, e.g. "s3", "sqs", "lambda", or "*".
	// For an event delivery it is the service receiving the delivery.
	Service string `json:"service"`
	// Source is the service making an event delivery, e.g. "sns" or
	// "events"; empty matches all. Only event deliveries have a source.
	Source string `json:"source,omitempty"`
	// Operation is the API operation, e.g. "PutItem"; empty matches all.
	// Event deliveries use the target's operation: Invoke, SendMessage,
	// Publish or StartExecution.
	Operation string `json:"operation,omitempty"`
	// Resource is matched as a substring of the request path, S3 bucket
	// and Lambda function name, or of an event delivery's source and target
	// ARNs; empty matches all.
	Resource string `json:"resource,omitempty"`
	// StartAfterSeconds delays when the rule becomes active, counted from
	// the start of the experiment.
	StartAfterSeconds int `json:"start_after_seconds,omitempty"`
	// DurationSeconds limits how long the rule stays active once it has
	// started; zero means until the experiment ends.
	DurationSeconds int `json:"duration_seconds,omitempty"`
	// Probability in (0, 1]; zero means 1 (always).
	Probability float64 `json:"probability,omitempty"`
	// MaxHits caps how many times the rule fires; zero is unlimited.
	MaxHits int          `json:"max_hits,omitempty"`
	Action  Action       `json:"action"`
	Error   *ErrorSpec   `json:"error,omitempty"`
	Latency *LatencySpec `json:"latency,omitempty"`
}

// Spec is the caller's description of an experiment.
type Spec struct {
	// Seed makes the experiment reproducible. Zero picks one, which is
	// reported back so the run can be replayed.
	Seed            int64  `json:"seed,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Rules           []Rule `json:"rules"`
}

const maxRules = 64

func invalid(code, format string, args ...any) *control.Error {
	return &control.Error{
		Kind:    control.KindInvalid,
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Hint:    "see docs/chaos.md for the experiment format",
	}
}

// normalise validates the spec against the known services and fills in
// defaults, returning a copy.
func (s Spec) normalise(known map[string]bool) (Spec, error) {
	if len(s.Rules) == 0 {
		return Spec{}, invalid("InvalidExperiment", "an experiment needs at least one rule")
	}
	if len(s.Rules) > maxRules {
		return Spec{}, invalid("InvalidExperiment", "an experiment can have at most %d rules", maxRules)
	}
	if s.DurationSeconds < 0 {
		return Spec{}, invalid("InvalidExperiment", "duration_seconds must not be negative")
	}

	out := Spec{Seed: s.Seed, DurationSeconds: s.DurationSeconds, Rules: make([]Rule, len(s.Rules))}
	ids := map[string]bool{}
	for i, r := range s.Rules {
		n, err := r.normalise(i, known)
		if err != nil {
			return Spec{}, err
		}
		if ids[n.ID] {
			return Spec{}, invalid("InvalidRule", "duplicate rule id %q", n.ID)
		}
		ids[n.ID] = true
		out.Rules[i] = n
	}
	return out, nil
}

func (r Rule) normalise(index int, known map[string]bool) (Rule, error) {
	if r.ID == "" {
		r.ID = fmt.Sprintf("rule-%d", index+1)
	}
	r.Service = strings.ToLower(strings.TrimSpace(r.Service))
	if r.Service == "" {
		return Rule{}, invalid("InvalidRule", "rule %q: service is required (use \"*\" for all services)", r.ID)
	}
	if r.Service != AnyService && known != nil && !known[r.Service] {
		return Rule{}, invalid("InvalidRule", "rule %q: unknown service %q", r.ID, r.Service)
	}
	r.Source = strings.ToLower(strings.TrimSpace(r.Source))
	if r.Source != "" && known != nil && !known[r.Source] {
		return Rule{}, invalid("InvalidRule", "rule %q: unknown source service %q", r.ID, r.Source)
	}
	if err := r.normaliseScope(); err != nil {
		return Rule{}, err
	}
	if r.StartAfterSeconds < 0 || r.DurationSeconds < 0 {
		return Rule{}, invalid("InvalidRule", "rule %q: start_after_seconds and duration_seconds must not be negative", r.ID)
	}
	if r.Probability == 0 {
		r.Probability = 1
	}
	if r.Probability < 0 || r.Probability > 1 {
		return Rule{}, invalid("InvalidRule", "rule %q: probability must be between 0 and 1", r.ID)
	}
	if r.MaxHits < 0 {
		return Rule{}, invalid("InvalidRule", "rule %q: max_hits must not be negative", r.ID)
	}

	// Copy nested specs so the engine never shares them with the caller.
	if r.Error != nil {
		e := *r.Error
		r.Error = &e
	}
	if r.Latency != nil {
		l := *r.Latency
		r.Latency = &l
	}

	switch r.Action {
	case ActionError:
		if r.Error == nil || r.Error.Code == "" || r.Error.StatusCode < 400 || r.Error.StatusCode > 599 {
			return Rule{}, invalid("InvalidRule", "rule %q: action \"error\" needs error.code and an error.status_code between 400 and 599", r.ID)
		}
		if r.Error.Message == "" {
			r.Error.Message = "injected fault"
		}
	case ActionThrottle, ActionInternalError, ActionUnavailable, ActionDrop:
	case ActionLatency:
		if r.Latency == nil || r.Latency.DelayMS <= 0 || r.Latency.JitterMS < 0 {
			return Rule{}, invalid("InvalidRule", "rule %q: action \"latency\" needs latency.delay_ms > 0", r.ID)
		}
		if time.Duration(r.Latency.DelayMS+r.Latency.JitterMS)*time.Millisecond > maxDelay {
			return Rule{}, invalid("InvalidRule", "rule %q: latency must not exceed %s", r.ID, maxDelay)
		}
	case ActionColdStart:
		if r.Service != "lambda" {
			return Rule{}, invalid("InvalidRule", "rule %q: action \"cold_start\" only applies to service \"lambda\"", r.ID)
		}
	case ActionFunctionError:
		if r.Service != "lambda" || (r.Operation != "" && !strings.EqualFold(r.Operation, "Invoke")) {
			return Rule{}, invalid("InvalidRule", "rule %q: action \"function_error\" only applies to lambda Invoke", r.ID)
		}
		r.Operation = "Invoke"
	case ActionDuplicate:
		if r.Scope != ScopeEvent {
			return Rule{}, invalid("InvalidRule", "rule %q: action \"duplicate\" only applies to scope \"event\"", r.ID)
		}
	default:
		return Rule{}, invalid("InvalidRule", "rule %q: unknown action %q", r.ID, r.Action)
	}
	return r, nil
}

func (r *Rule) normaliseScope() error {
	r.Scope = Scope(strings.ToLower(strings.TrimSpace(string(r.Scope))))
	switch r.Scope {
	case "":
		r.Scope = ScopeAPI
		if r.Source != "" {
			r.Scope = ScopeEvent
		}
	case ScopeAPI, ScopeEvent, ScopeAll:
	default:
		return invalid("InvalidRule", "rule %q: scope must be \"api\", \"event\" or \"all\"", r.ID)
	}
	if r.Source != "" && r.Scope == ScopeAPI {
		return invalid("InvalidRule", "rule %q: source only applies to event deliveries; set scope to \"event\"", r.ID)
	}
	return nil
}

const maxDelay = 5 * time.Minute

// matches reports whether the rule targets the traffic.
func (r Rule) matches(t traffic) bool {
	if r.Scope != ScopeAll && r.Scope != t.scope {
		return false
	}
	if r.Service != AnyService && r.Service != t.service {
		return false
	}
	if r.Source != "" && r.Source != t.source {
		return false
	}
	if r.Operation != "" && !strings.EqualFold(r.Operation, t.operation) {
		return false
	}
	if r.Resource != "" && !t.mentions(r.Resource) {
		return false
	}
	return true
}

// activeAt reports whether the rule's time window, counted from the start
// of the experiment, contains now.
func (r Rule) activeAt(startedAt, now time.Time) bool {
	from := startedAt.Add(time.Duration(r.StartAfterSeconds) * time.Second)
	if now.Before(from) {
		return false
	}
	return r.DurationSeconds == 0 || now.Before(from.Add(time.Duration(r.DurationSeconds)*time.Second))
}

// errorFor turns an error action into a concrete AWS error for service.
func (r Rule) errorFor(service string) *ErrorSpec {
	switch r.Action {
	case ActionError:
		e := *r.Error
		return &e
	case ActionThrottle:
		if service == "s3" {
			return &ErrorSpec{StatusCode: http.StatusServiceUnavailable, Code: "SlowDown", Message: "Please reduce your request rate. (injected fault)"}
		}
		return &ErrorSpec{StatusCode: http.StatusBadRequest, Code: "ThrottlingException", Message: "Rate exceeded (injected fault)"}
	case ActionInternalError:
		code := "InternalFailure"
		if service == "s3" {
			code = "InternalError"
		}
		return &ErrorSpec{StatusCode: http.StatusInternalServerError, Code: code, Message: "We encountered an internal error. Please try again. (injected fault)"}
	case ActionUnavailable:
		return &ErrorSpec{StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailable", Message: "Service is unavailable. (injected fault)"}
	default:
		return nil
	}
}
