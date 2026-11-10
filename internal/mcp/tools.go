package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// ControlAPI is the part of the Stratus control client the tools use.
type ControlAPI interface {
	Endpoint() string
	Health(ctx context.Context) (json.RawMessage, error)
	Reset(ctx context.Context, services ...string) (json.RawMessage, error)
	CreateSnapshot(ctx context.Context, name string) (json.RawMessage, error)
	ListSnapshots(ctx context.Context) (json.RawMessage, error)
	RestoreSnapshot(ctx context.Context, name string) (json.RawMessage, error)
	DeleteSnapshot(ctx context.Context, name string) error
	StartChaos(ctx context.Context, spec json.RawMessage) (json.RawMessage, error)
	ChaosReport(ctx context.Context) (json.RawMessage, error)
	StopChaos(ctx context.Context) (json.RawMessage, error)
	Diagnose(ctx context.Context, query url.Values) (json.RawMessage, error)
	SESMessages(ctx context.Context, query url.Values) (json.RawMessage, error)
	SchedulerAdvance(ctx context.Context, by, to string) (json.RawMessage, error)
	SchedulerRun(ctx context.Context, group, name string) (json.RawMessage, error)
	BedrockAddFixture(ctx context.Context, fixture json.RawMessage) (json.RawMessage, error)
	BedrockInvocations(ctx context.Context, query url.Values) (json.RawMessage, error)
	KafkaClusters(ctx context.Context) (json.RawMessage, error)
}

// Instructions are sent to the client at initialization.
const Instructions = `Stratus is a local AWS emulator. Point AWS SDKs and the AWS CLI at it with the
environment from stratus_connection_info. Typical loop: stratus_status to check it is
ready, stratus_snapshot_save after seeding, run tests, stratus_diagnose when something
fails (it says whether a failure is your bug, an injected chaos fault, or an emulator
gap), stratus_snapshot_restore to get back to the seeded state. Use stratus_chaos_start
to inject seeded, reproducible faults. Emails sent through SES land in a captured inbox
(stratus_ses_inbox); EventBridge Scheduler runs on a clock you can fast-forward with
stratus_scheduler_advance, so tests never wait for real time. Bedrock model calls are
answered from fixtures: add one with stratus_bedrock_fixture_add, and see every prompt
the application sent (and which calls had no fixture) with stratus_bedrock_invocations.
MSK clusters are real Kafka brokers: stratus_kafka_clusters gives each one's bootstrap address.`

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

type nameArg struct {
	Name string `json:"name"`
}

func requireName(raw json.RawMessage) (string, error) {
	a, err := decode[nameArg](raw)
	if err != nil {
		return "", err
	}
	if a.Name == "" {
		return "", errors.New("invalid arguments: name is required")
	}
	return a.Name, nil
}

// Tools returns the Stratus tools backed by api.
func Tools(api ControlAPI) []Tool {
	return []Tool{
		{
			Name:        "stratus_connection_info",
			Description: "Return the Stratus endpoint and the environment variables that point AWS SDKs and the AWS CLI at it.",
			InputSchema: schema(map[string]any{}),
			Handler: func(context.Context, json.RawMessage) (any, error) {
				ep := api.Endpoint()
				return map[string]any{
					"endpoint": ep,
					"env": map[string]string{
						"AWS_ENDPOINT_URL":      ep,
						"STRATUS_ENDPOINT":      ep,
						"AWS_ACCESS_KEY_ID":     "test",
						"AWS_SECRET_ACCESS_KEY": "test",
						"AWS_REGION":            "us-east-1",
						"AWS_DEFAULT_REGION":    "us-east-1",
					},
					"note": "Stratus accepts any credentials. Lambda functions reach Stratus at http://host.docker.internal:<port>.",
				}, nil
			},
		},
		{
			Name:        "stratus_status",
			Description: "Check whether Stratus is healthy and ready, which services it emulates, and whether Docker (needed only for Lambda) is available.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.Health(ctx)
			},
		},
		{
			Name: "stratus_diagnose",
			Description: "Explain recent failed AWS requests. Groups failures by service, operation and error code, and for each gives the likely cause, " +
				"the next step, and your_bug: false when the failure is an injected chaos fault or an operation Stratus does not emulate (do not change application code for those). " +
				"Call this first when a test against Stratus fails.",
			InputSchema: schema(map[string]any{
				"since":            str("Only failures this recent: a duration like \"5m\" or an RFC 3339 time."),
				"service":          str("Only this AWS service, e.g. \"sqs\"."),
				"exclude_injected": map[string]any{"type": "boolean", "description": "Leave out failures caused by chaos rules."},
				"max_groups":       map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Since           string `json:"since"`
					Service         string `json:"service"`
					ExcludeInjected bool   `json:"exclude_injected"`
					MaxGroups       int    `json:"max_groups"`
				}](raw)
				if err != nil {
					return nil, err
				}
				q := url.Values{}
				if a.Since != "" {
					q.Set("since", a.Since)
				}
				if a.Service != "" {
					q.Set("service", a.Service)
				}
				if a.ExcludeInjected {
					q.Set("exclude_injected", "true")
				}
				if a.MaxGroups > 0 {
					q.Set("max_groups", strconv.Itoa(a.MaxGroups))
				}
				return api.Diagnose(ctx, q)
			},
		},
		{
			Name:        "stratus_reset",
			Description: "Delete all emulated state, or only the named services' state (e.g. [\"s3\", \"sqs\"]). Snapshots are kept.",
			InputSchema: schema(map[string]any{
				"services": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Services to reset; omit to reset everything."},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Services []string `json:"services"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return api.Reset(ctx, a.Services...)
			},
		},
		{
			Name:        "stratus_snapshot_save",
			Description: "Capture all current emulated state under a name, so it can be restored later, including the rows and keys inside RDS and ElastiCache engines. Fast and cheap; take one after seeding test data.",
			InputSchema: schema(map[string]any{"name": str("Lowercase name such as \"seeded\"; omit to generate one.")}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[nameArg](raw)
				if err != nil {
					return nil, err
				}
				return api.CreateSnapshot(ctx, a.Name)
			},
		},
		{
			Name:        "stratus_snapshot_restore",
			Description: "Replace all current emulated state with a saved snapshot. Use it to undo a test's side effects.",
			InputSchema: schema(map[string]any{"name": str("Snapshot name.")}, "name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				name, err := requireName(raw)
				if err != nil {
					return nil, err
				}
				return api.RestoreSnapshot(ctx, name)
			},
		},
		{
			Name:        "stratus_snapshot_list",
			Description: "List saved snapshots, oldest first, with the services that held state in each.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.ListSnapshots(ctx)
			},
		},
		{
			Name:        "stratus_snapshot_delete",
			Description: "Delete a saved snapshot. Current state is not affected.",
			InputSchema: schema(map[string]any{"name": str("Snapshot name.")}, "name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				name, err := requireName(raw)
				if err != nil {
					return nil, err
				}
				if err := api.DeleteSnapshot(ctx, name); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": name}, nil
			},
		},
		{
			Name: "stratus_chaos_start",
			Description: "Start a seeded chaos experiment that injects faults into AWS calls and internal event deliveries (replaces any running one). " +
				"Each rule has service (or \"*\"), optional operation and resource substring, optional probability (0-1) and max_hits, and an action: " +
				"throttle, internal_error, unavailable, error (with error.status_code and error.code), latency (with latency.delay_ms and optional jitter_ms), drop, cold_start (lambda only), " +
				"function_error (lambda Invoke fails like a throwing function) or duplicate (event scope only). " +
				"scope \"event\" (or setting source, e.g. \"sns\", \"sqs\", \"events\", \"stepfunctions\") targets deliveries such as SQS-to-Lambda, SNS fan-out and EventBridge targets, where service is the receiving service; scope \"all\" targets both. " +
				"The same seed replays the same faults.",
			InputSchema: schema(map[string]any{
				"seed":             map[string]any{"type": "integer", "description": "Reuse a seed to replay a run; omit to get a new one."},
				"duration_seconds": map[string]any{"type": "integer", "minimum": 0},
				"rules": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":                  str("Rule name shown in reports."),
							"scope":               map[string]any{"type": "string", "enum": []string{"api", "event", "all"}, "description": "api (default) for AWS API calls, event for internal deliveries, all for both."},
							"source":              str("Service making an event delivery, such as \"sns\" or \"sqs\"; implies scope event."),
							"start_after_seconds": map[string]any{"type": "integer", "minimum": 0, "description": "Delay before the rule becomes active."},
							"duration_seconds":    map[string]any{"type": "integer", "minimum": 0, "description": "How long the rule stays active."},
							"service":             str("AWS service such as \"dynamodb\", or \"*\". For event deliveries, the receiving service."),
							"operation":           str("API operation such as \"PutItem\"."),
							"resource":            str("Substring of the path, bucket or function name, or of a delivery's source or target ARN."),
							"probability":         map[string]any{"type": "number", "minimum": 0, "maximum": 1},
							"max_hits":            map[string]any{"type": "integer", "minimum": 0},
							"action":              map[string]any{"type": "string", "enum": []string{"throttle", "internal_error", "unavailable", "error", "latency", "drop", "cold_start", "function_error", "duplicate"}},
							"error": map[string]any{"type": "object", "properties": map[string]any{
								"status_code": map[string]any{"type": "integer"}, "code": str("AWS error code."), "message": str("Error message."),
							}},
							"latency": map[string]any{"type": "object", "properties": map[string]any{
								"delay_ms": map[string]any{"type": "integer"}, "jitter_ms": map[string]any{"type": "integer"},
							}},
						},
						"required": []string{"service", "action"},
					},
				},
			}, "rules"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				return api.StartChaos(ctx, raw)
			},
		},
		{
			Name:        "stratus_chaos_status",
			Description: "Report the running chaos experiment: per-rule match and fault counts, and the latest faulted requests with request IDs.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.ChaosReport(ctx)
			},
		},
		{
			Name:        "stratus_chaos_stop",
			Description: "Stop the chaos experiment and return its final report, including the seed to replay it.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.StopChaos(ctx)
			},
		},
		{
			Name: "stratus_ses_inbox",
			Description: "List emails the application sent through SES (v1 or v2). Nothing is delivered for real: every send lands here with its " +
				"subject, text and HTML body, recipients and tags, oldest first. Filter by recipient, sender, subject substring or time.",
			InputSchema: schema(map[string]any{
				"to":      str("Only emails addressed to this recipient (To, Cc or Bcc)."),
				"from":    str("Only emails from this sender address."),
				"subject": str("Only emails whose subject contains this text (case-insensitive)."),
				"since":   str("Only emails this recent: a duration like \"5m\" or an RFC 3339 time."),
				"limit":   map[string]any{"type": "integer", "minimum": 1, "description": "Return only the newest N emails."},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					To      string `json:"to"`
					From    string `json:"from"`
					Subject string `json:"subject"`
					Since   string `json:"since"`
					Limit   int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				q := url.Values{}
				for key, value := range map[string]string{"to": a.To, "from": a.From, "subject": a.Subject, "since": a.Since} {
					if value != "" {
						q.Set(key, value)
					}
				}
				if a.Limit > 0 {
					q.Set("limit", fmt.Sprint(a.Limit))
				}
				return api.SESMessages(ctx, q)
			},
		},
		{
			Name: "stratus_scheduler_advance",
			Description: "Move the EventBridge Scheduler clock forward and fire every schedule that became due, once each, before returning. " +
				"Use it instead of sleeping: advance by \"1h\" to see an hourly schedule deliver to its queue, topic, function or state machine. " +
				"Returns the new scheduler time and what fired, including any delivery error.",
			InputSchema: schema(map[string]any{
				"by": str("How far to advance, as a duration such as \"30m\" or \"2h\"."),
				"to": str("Advance to this RFC 3339 time instead; must be ahead of the scheduler clock."),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					By string `json:"by"`
					To string `json:"to"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return api.SchedulerAdvance(ctx, a.By, a.To)
			},
		},
		{
			Name:        "stratus_scheduler_run",
			Description: "Fire one EventBridge schedule right now, whatever its expression or state says, and report the delivery outcome.",
			InputSchema: schema(map[string]any{
				"name":  str("The schedule name."),
				"group": str("Its schedule group; defaults to \"default\"."),
			}, "name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Name  string `json:"name"`
					Group string `json:"group"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return api.SchedulerRun(ctx, a.Group, a.Name)
			},
		},
		{
			Name: "stratus_bedrock_fixture_add",
			Description: "Make Bedrock answer the application's model calls with a fixed reply, so code that calls an LLM runs offline and " +
				"deterministically. The fixture answers Converse, ConverseStream, InvokeModel and InvokeModelWithResponseStream calls whose " +
				"model and prompt match, in the model's own response format (Anthropic, Nova, Titan, Llama, Mistral). Fixtures added here " +
				"come before fixture files and last until reset.",
			InputSchema: schema(map[string]any{
				"text":     str("The model's reply."),
				"model":    str("Model id, inference profile or pattern with * (e.g. \"anthropic.claude-*\"). Omit to answer every model."),
				"contains": str("Only answer prompts containing this text (system prompt, messages and tool results). Omit to answer every prompt."),
				"kind":     map[string]any{"type": "string", "enum": []string{"converse", "invoke"}, "description": "Only answer Converse or only InvokeModel calls. Required with response."},
				"response": map[string]any{"type": "object", "description": "An exact response body instead of text: a Converse response (e.g. with a toolUse block) for kind converse, the provider's body for kind invoke."},
				"id":       str("Optional id, to delete the fixture later."),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Text     string          `json:"text"`
					Model    string          `json:"model"`
					Contains string          `json:"contains"`
					Kind     string          `json:"kind"`
					Response json.RawMessage `json:"response"`
					ID       string          `json:"id"`
				}](raw)
				if err != nil {
					return nil, err
				}
				fixture := map[string]any{}
				for key, value := range map[string]string{"text": a.Text, "model": a.Model, "kind": a.Kind, "id": a.ID} {
					if value != "" {
						fixture[key] = value
					}
				}
				if a.Contains != "" {
					fixture["match"] = map[string]string{"contains": a.Contains}
				}
				if len(a.Response) > 0 {
					fixture["response"] = a.Response
				}
				body, err := json.Marshal(fixture)
				if err != nil {
					return nil, err
				}
				return api.BedrockAddFixture(ctx, body)
			},
		},
		{
			Name: "stratus_bedrock_invocations",
			Description: "List the Bedrock model calls the application made, oldest first: the prompt it sent, the reply, the fixture that " +
				"answered and the outcome (replayed, recorded, synthesized for embeddings, missed when no fixture matched, failed). Use it to " +
				"check what your code actually asked the model, and to find calls that need a fixture.",
			InputSchema: schema(map[string]any{
				"model":    str("Only calls to this model id or pattern with *."),
				"contains": str("Only calls whose prompt contains this text."),
				"outcome":  map[string]any{"type": "string", "enum": []string{"replayed", "recorded", "synthesized", "missed", "failed"}, "description": "Only calls with this outcome."},
				"limit":    map[string]any{"type": "integer", "minimum": 1, "description": "Return only the newest N calls."},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Model    string `json:"model"`
					Contains string `json:"contains"`
					Outcome  string `json:"outcome"`
					Limit    int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				q := url.Values{}
				for key, value := range map[string]string{"model": a.Model, "contains": a.Contains, "outcome": a.Outcome} {
					if value != "" {
						q.Set(key, value)
					}
				}
				if a.Limit > 0 {
					q.Set("limit", fmt.Sprint(a.Limit))
				}
				return api.BedrockInvocations(ctx, q)
			},
		},
		{
			Name: "stratus_kafka_clusters",
			Description: "List MSK clusters with the bootstrap broker address of each (host:port). Every cluster is a real Kafka-compatible broker, " +
				"so pass the address to any Kafka client or CLI to create topics, produce and consume.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.KafkaClusters(ctx)
			},
		},
	}
}
