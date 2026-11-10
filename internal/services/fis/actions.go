package fis

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/scope"
)

// Resource types Stratus can target.
const (
	ResourceLambdaFunction = "aws:lambda:function"
	ResourceIAMRole        = "aws:iam:role"
)

// Parameter describes an action parameter, for GetAction.
type Parameter struct {
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// ActionTarget names the resource type an action target key accepts.
type ActionTarget struct {
	ResourceType string `json:"resourceType"`
}

// ActionDefinition is an FIS action Stratus supports. Adding an action is
// adding a definition: nothing else in the package needs to change.
type ActionDefinition struct {
	ID          string                  `json:"id"`
	Arn         string                  `json:"arn"`
	Description string                  `json:"description"`
	Parameters  map[string]Parameter    `json:"parameters,omitempty"`
	Targets     map[string]ActionTarget `json:"targets,omitempty"`

	// rules turns an action into chaos rules. selected maps each target
	// key to the resources it selected; nil selects every resource.
	rules func(params params, selected map[string][]string) ([]chaos.Rule, error)
}

// ActionSummary is an action as ListActions shows it.
type ActionSummary struct {
	ID          string                  `json:"id"`
	Arn         string                  `json:"arn"`
	Description string                  `json:"description"`
	Targets     map[string]ActionTarget `json:"targets,omitempty"`
}

func (d ActionDefinition) summary() ActionSummary {
	return ActionSummary{ID: d.ID, Arn: d.Arn, Description: d.Description, Targets: d.Targets}
}

// params reads an action's string parameters.
type params map[string]string

func (p params) duration() (time.Duration, error) {
	raw, ok := p["duration"]
	if !ok || raw == "" {
		return 0, nil
	}
	d, ok := parseDuration(raw)
	if !ok {
		return 0, fmt.Errorf("duration %q is not an ISO 8601 duration such as PT5M", raw)
	}
	return d, nil
}

// probability reads a percentage parameter (0-100], defaulting to 100.
func (p params) probability(key string) (float64, error) {
	raw, ok := p[key]
	if !ok || raw == "" {
		return 1, nil
	}
	pct, err := strconv.ParseFloat(raw, 64)
	if err != nil || pct <= 0 || pct > 100 {
		return 0, fmt.Errorf("%s must be a number in (0, 100], got %q", key, raw)
	}
	return pct / 100, nil
}

func (p params) intOr(key string, fallback int) (int, error) {
	raw, ok := p[key]
	if !ok || raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return n, nil
}

func (p params) required(key string) (string, error) {
	v := strings.TrimSpace(p[key])
	if v == "" {
		return "", fmt.Errorf("parameter %q is required", key)
	}
	return v, nil
}

var (
	durationParam = Parameter{Description: "How long the action runs, as an ISO 8601 duration such as PT5M. Without it the action runs until the experiment is stopped."}
	lambdaTargets = map[string]ActionTarget{"Functions": {ResourceType: ResourceLambdaFunction}}
	roleTargets   = map[string]ActionTarget{"Roles": {ResourceType: ResourceIAMRole}}
)

// perFunction makes one rule per selected function, or one for all.
func perFunction(selected map[string][]string, rule chaos.Rule) []chaos.Rule {
	functions := selected["Functions"]
	if functions == nil {
		return []chaos.Rule{rule}
	}
	rules := make([]chaos.Rule, 0, len(functions))
	for _, fn := range functions {
		r := rule
		r.Resource = fn
		rules = append(rules, r)
	}
	return rules
}

func lambdaInvocationError(p params, selected map[string][]string) ([]chaos.Rule, error) {
	prob, err := p.probability("invocationPercentage")
	if err != nil {
		return nil, err
	}
	if v := p["preventExecution"]; v != "" && v != "true" {
		return nil, fmt.Errorf("Stratus only supports preventExecution=true: the function is not run")
	}
	return perFunction(selected, chaos.Rule{
		Scope: chaos.ScopeAll, Service: "lambda", Operation: "Invoke", Probability: prob, Action: chaos.ActionFunctionError,
	}), nil
}

func lambdaAddDelay(p params, selected map[string][]string) ([]chaos.Rule, error) {
	prob, err := p.probability("invocationPercentage")
	if err != nil {
		return nil, err
	}
	delay, err := p.intOr("startupDelayMilliseconds", 1000)
	if err != nil {
		return nil, err
	}
	if delay == 0 {
		return nil, fmt.Errorf("startupDelayMilliseconds must be positive")
	}
	return perFunction(selected, chaos.Rule{
		Scope: chaos.ScopeAll, Service: "lambda", Operation: "Invoke", Probability: prob,
		Action: chaos.ActionLatency, Latency: &chaos.LatencySpec{DelayMS: delay},
	}), nil
}

// injectAPI builds the aws:fis:inject-api-* actions: errors on a service's
// API operations. FIS targets IAM roles; Stratus has no caller identity to
// match, so the faults apply to every caller.
func injectAPI(action chaos.Action) func(params, map[string][]string) ([]chaos.Rule, error) {
	return func(p params, _ map[string][]string) ([]chaos.Rule, error) {
		service, err := p.required("service")
		if err != nil {
			return nil, err
		}
		prob, err := p.probability("percentage")
		if err != nil {
			return nil, err
		}
		base := chaos.Rule{Scope: chaos.ScopeAPI, Service: strings.ToLower(service), Probability: prob, Action: action}
		var ops []string
		for _, op := range strings.Split(p["operations"], ",") {
			if op = strings.TrimSpace(op); op != "" {
				ops = append(ops, op)
			}
		}
		if len(ops) == 0 {
			return []chaos.Rule{base}, nil
		}
		rules := make([]chaos.Rule, 0, len(ops))
		for _, op := range ops {
			r := base
			r.Operation = op
			rules = append(rules, r)
		}
		return rules, nil
	}
}

// delivery builds the stratus:delivery:* actions, which fault the events
// Stratus delivers internally. FIS has no equivalent because in AWS these
// paths are not reachable by a fault injector.
func delivery(action chaos.Action) func(params, map[string][]string) ([]chaos.Rule, error) {
	return func(p params, _ map[string][]string) ([]chaos.Rule, error) {
		prob, err := p.probability("percentage")
		if err != nil {
			return nil, err
		}
		service := strings.ToLower(strings.TrimSpace(p["service"]))
		if service == "" {
			service = chaos.AnyService
		}
		rule := chaos.Rule{
			Scope: chaos.ScopeEvent, Service: service, Source: strings.ToLower(strings.TrimSpace(p["source"])),
			Operation: p["operation"], Resource: p["resource"], Probability: prob, Action: action,
		}
		switch action {
		case chaos.ActionLatency:
			delay, err := p.intOr("delayMilliseconds", 1000)
			if err != nil {
				return nil, err
			}
			jitter, err := p.intOr("jitterMilliseconds", 0)
			if err != nil {
				return nil, err
			}
			rule.Latency = &chaos.LatencySpec{DelayMS: delay, JitterMS: jitter}
		case chaos.ActionError:
			status, err := p.intOr("statusCode", http.StatusInternalServerError)
			if err != nil {
				return nil, err
			}
			code := p["errorCode"]
			if code == "" {
				code = "InternalFailure"
			}
			rule.Error = &chaos.ErrorSpec{StatusCode: status, Code: code, Message: p["errorMessage"]}
		}
		return []chaos.Rule{rule}, nil
	}
}

func deliveryParams(extra map[string]Parameter) map[string]Parameter {
	out := map[string]Parameter{
		"source":     {Description: "Service making the delivery: sqs, kinesis, dynamodb, sns, events, stepfunctions or lambda. Empty matches all."},
		"service":    {Description: "Service receiving the delivery: lambda, sqs, sns or stepfunctions. Empty matches all."},
		"operation":  {Description: "Target operation: Invoke, SendMessage, Publish or StartExecution. Empty matches all."},
		"resource":   {Description: "Substring of the source or target ARN, or of the target function name."},
		"percentage": {Description: "Percentage of matching deliveries to fault, (0, 100]. Defaults to 100."},
		"duration":   durationParam,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func apiParams() map[string]Parameter {
	return map[string]Parameter{
		"service":    {Description: "Service whose API to fault, such as dynamodb, s3, sqs or kinesis.", Required: true},
		"operations": {Description: "Comma-separated API operations, such as PutItem,GetItem. Empty faults every operation."},
		"percentage": {Description: "Percentage of matching requests to fault, (0, 100]. Defaults to 100."},
		"duration":   durationParam,
	}
}

func actionArn(sc scope.Scope, id string) string {
	return sc.ARN("fis", "action/"+id)
}

func newCatalog(sc scope.Scope) map[string]ActionDefinition {
	defs := []ActionDefinition{
		{
			ID:          "aws:fis:wait",
			Description: "Waits for the given duration. Use it with startAfter to sequence other actions.",
			Parameters:  map[string]Parameter{"duration": {Description: "How long to wait, as an ISO 8601 duration.", Required: true}},
			rules:       func(params, map[string][]string) ([]chaos.Rule, error) { return nil, nil },
		},
		{
			ID:          "aws:lambda:invocation-error",
			Description: "Makes Lambda invocations of the target functions fail with a function error, without running them. Applies to Invoke calls and to event-driven invocations.",
			Targets:     lambdaTargets,
			Parameters: map[string]Parameter{
				"duration":             durationParam,
				"invocationPercentage": {Description: "Percentage of invocations to fail, (0, 100]. Defaults to 100."},
				"preventExecution":     {Description: "Must be true (the default): the function is not run."},
			},
			rules: lambdaInvocationError,
		},
		{
			ID:          "aws:lambda:invocation-add-delay",
			Description: "Delays Lambda invocations of the target functions before they run. Applies to Invoke calls and to event-driven invocations.",
			Targets:     lambdaTargets,
			Parameters: map[string]Parameter{
				"duration":                 durationParam,
				"invocationPercentage":     {Description: "Percentage of invocations to delay, (0, 100]. Defaults to 100."},
				"startupDelayMilliseconds": {Description: "Delay in milliseconds. Defaults to 1000."},
			},
			rules: lambdaAddDelay,
		},
		{ID: "aws:fis:inject-api-internal-error", Description: "Returns internal errors from a service's API operations.", Targets: roleTargets, Parameters: apiParams(), rules: injectAPI(chaos.ActionInternalError)},
		{ID: "aws:fis:inject-api-throttle-error", Description: "Returns throttling errors from a service's API operations.", Targets: roleTargets, Parameters: apiParams(), rules: injectAPI(chaos.ActionThrottle)},
		{ID: "aws:fis:inject-api-unavailable-error", Description: "Returns service unavailable errors from a service's API operations.", Targets: roleTargets, Parameters: apiParams(), rules: injectAPI(chaos.ActionUnavailable)},
		{ID: "stratus:delivery:error", Description: "Stratus extension. Fails internal event deliveries (event source mappings, SNS fan-out, EventBridge targets, Step Functions tasks, Lambda destinations).", Parameters: deliveryParams(map[string]Parameter{
			"statusCode":   {Description: "HTTP status of the error. Defaults to 500."},
			"errorCode":    {Description: "AWS error code. Defaults to InternalFailure."},
			"errorMessage": {Description: "Error message."},
		}), rules: delivery(chaos.ActionError)},
		{ID: "stratus:delivery:drop", Description: "Stratus extension. Loses internal event deliveries: the target never sees them.", Parameters: deliveryParams(nil), rules: delivery(chaos.ActionDrop)},
		{ID: "stratus:delivery:duplicate", Description: "Stratus extension. Delivers internal events twice, as at-least-once delivery can.", Parameters: deliveryParams(nil), rules: delivery(chaos.ActionDuplicate)},
		{ID: "stratus:delivery:delay", Description: "Stratus extension. Delays internal event deliveries; delayed async events can arrive out of order.", Parameters: deliveryParams(map[string]Parameter{
			"delayMilliseconds":  {Description: "Delay in milliseconds. Defaults to 1000."},
			"jitterMilliseconds": {Description: "Deterministic extra delay of up to this many milliseconds."},
		}), rules: delivery(chaos.ActionLatency)},
	}
	catalog := make(map[string]ActionDefinition, len(defs))
	for _, d := range defs {
		d.Arn = actionArn(sc, d.ID)
		catalog[d.ID] = d
	}
	return catalog
}

// TargetResourceType is a resource type, for ListTargetResourceTypes.
type TargetResourceType struct {
	ResourceType string `json:"resourceType"`
	Description  string `json:"description"`
}

var targetResourceTypes = []TargetResourceType{
	{ResourceType: ResourceLambdaFunction, Description: "Lambda functions, selected by resourceArns. Without resourceArns, every function."},
	{ResourceType: ResourceIAMRole, Description: "IAM roles. Accepted for aws:fis:inject-api-* actions; Stratus applies their faults to every caller."},
}

func sortedActions(catalog map[string]ActionDefinition) []ActionDefinition {
	out := make([]ActionDefinition, 0, len(catalog))
	for _, d := range catalog {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
