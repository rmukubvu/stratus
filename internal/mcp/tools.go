package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/stratus/internal/awsmodel"
	"github.com/stratus/internal/check"
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
	SetLimits(ctx context.Context, spec json.RawMessage) (json.RawMessage, error)
	LimitsStatus(ctx context.Context) (json.RawMessage, error)
	ClearLimits(ctx context.Context) (json.RawMessage, error)
	SetSigV4(ctx context.Context, mode string) (json.RawMessage, error)
	SigV4Status(ctx context.Context) (json.RawMessage, error)
	StartRun(ctx context.Context, spec json.RawMessage) (json.RawMessage, error)
	GetRun(ctx context.Context, id string) (json.RawMessage, error)
	StopRun(ctx context.Context, id string) (json.RawMessage, error)
	Diagnose(ctx context.Context, query url.Values) (json.RawMessage, error)
	SESMessages(ctx context.Context, query url.Values) (json.RawMessage, error)
	ClockStatus(ctx context.Context) (json.RawMessage, error)
	ClockAdvance(ctx context.Context, by, to string) (json.RawMessage, error)
	ClockFreeze(ctx context.Context) (json.RawMessage, error)
	ClockResume(ctx context.Context) (json.RawMessage, error)
	SchedulerAdvance(ctx context.Context, by, to string) (json.RawMessage, error)
	SchedulerRun(ctx context.Context, group, name string) (json.RawMessage, error)
	BedrockAddFixture(ctx context.Context, fixture json.RawMessage) (json.RawMessage, error)
	BedrockInvocations(ctx context.Context, query url.Values) (json.RawMessage, error)
	KafkaClusters(ctx context.Context) (json.RawMessage, error)
	DNSResolve(ctx context.Context, name, rtype string) (json.RawMessage, error)
	AdviseFamilies(ctx context.Context) (json.RawMessage, error)
	AdviseStart(ctx context.Context, in json.RawMessage) (json.RawMessage, error)
	AdviseList(ctx context.Context) (json.RawMessage, error)
	AdviseGet(ctx context.Context, id string) (json.RawMessage, error)
	AdviseAnswer(ctx context.Context, id string, answers map[string]any) (json.RawMessage, error)
	AdviseAssess(ctx context.Context, id string) (json.RawMessage, error)
	AdviseDecide(ctx context.Context, id, option, rationale string) (json.RawMessage, error)
	AdviseADR(ctx context.Context, id string) (string, error)
}

// Instructions are sent to the client at initialization.
const Instructions = `Stratus is a local AWS emulator. Point AWS SDKs and the AWS CLI at it with the
environment from stratus_connection_info. Typical loop: stratus_status to check it is
ready, stratus_snapshot_save after seeding, run tests, stratus_diagnose when something
fails (it says whether a failure is your bug, an injected chaos fault, or an emulator
gap), stratus_snapshot_restore to get back to the seeded state. Use stratus_chaos_start
to inject seeded, reproducible faults. Emails sent through SES land in a captured inbox
(stratus_ses_inbox). Never sleep to wait for time-based behaviour: stratus_clock_advance moves
the emulator's clock forward (SQS visibility timeouts and delays, alarm periods, Object Lock
retention, schedules) and fires due EventBridge schedules before returning;
stratus_clock_freeze holds time still so timestamps are predictable. Bedrock model calls are
answered from fixtures: add one with stratus_bedrock_fixture_add, and see every prompt
the application sent (and which calls had no fixture) with stratus_bedrock_invocations.
MSK clusters are real Kafka brokers: stratus_kafka_clusters gives each one's bootstrap address.
Route 53 zones and the records Cloud Map registers can be looked up with stratus_dns_resolve.
Before relying on an AWS operation, stratus_coverage says whether Stratus implements it, and
stratus_check scans a codebase for calls Stratus would answer with 501. Requests that break
AWS's input constraints get AWS's own ValidationException, so fix the input rather than the test.
When someone is choosing AWS services for a workload (which queue, stream or event bus), guide the
decision with stratus_advise_start: ask the person its questions, stratus_advise_assess to rank the
options with reasons tied to AWS quotas, let them choose (stratus_advise_decide), and write the ADR
from stratus_advise_adr into the repository.`

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
	return append(coreTools(api), adviseTools(api)...)
}

func coreTools(api ControlAPI) []Tool {
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
						// Keeps host-prefixed operations (Cloud Map's
						// DiscoverInstances) on the Stratus host.
						"AWS_DISABLE_HOST_PREFIX_INJECTION": "true",
					},
					"note": "Stratus accepts any credentials and signature by default; stratus_sigv4_set strict verifies signatures as AWS does. " +
						"Lambda functions get AWS_ENDPOINT_URL pointing back at Stratus automatically, and what they print lands in CloudWatch Logs under /aws/lambda/<function>.",
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
			Name: "stratus_limits_set",
			Description: "Enforce AWS service quotas so the app meets throttling under load, with the same errors AWS returns (SlowDown for S3, ThrottlingException, TooManyRequestsException). " +
				"profile is the base: aws-default (documented default quotas: S3 per prefix, SSM Get*, KMS crypto, Secrets Manager reads), strict (the same at 1%, to expose missing retries with little load) or none. " +
				"scale multiplies every rate, so a laptop-sized load reaches the same limits a production load would. " +
				"rates adds rules or replaces base rules by id; disabled turns a base rule off. " +
				"dynamodb tunes table capacity (a provisioned table gets its ProvisionedThroughput and banks burst_seconds of unused capacity, 300 by default; burst_seconds 0 throttles it as soon as it exceeds its rate) and lambda tunes account concurrency; " +
				"aws-default and strict enforce both, none only when the section is given. " +
				"Replaces any active profile; throttled requests carry X-Stratus-Limit and show as limit_exceeded in stratus_diagnose.",
			InputSchema: schema(map[string]any{
				"profile": map[string]any{"type": "string", "enum": []string{"aws-default", "strict", "none"}},
				"scale":   map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Multiplies every rate; 0.01 makes 5,500/s into 55/s."},
				"dynamodb": map[string]any{
					"type":        "object",
					"description": "DynamoDB table capacity model.",
					"properties": map[string]any{
						"disabled":              map[string]any{"type": "boolean", "description": "Turn capacity off while the other limits apply."},
						"burst_seconds":         map[string]any{"type": "number", "minimum": 0, "description": "Seconds of unused provisioned capacity a table banks; default 300 as in AWS, 0 banks none."},
						"partition_read_units":  map[string]any{"type": "number", "minimum": 0, "description": "Read units one partition serves per second before scaling; default 3000."},
						"partition_write_units": map[string]any{"type": "number", "minimum": 0, "description": "Write units one partition serves per second before scaling; default 1000."},
						"on_demand_read_units":  map[string]any{"type": "number", "minimum": 0, "description": "An on-demand table's maximum reads per second before scaling; default 40000."},
						"on_demand_write_units": map[string]any{"type": "number", "minimum": 0, "description": "An on-demand table's maximum writes per second before scaling; default 40000."},
						"tables": map[string]any{
							"type":                 "object",
							"description":          "Partition count by table name, e.g. {\"orders\": {\"partitions\": 8}}.",
							"additionalProperties": map[string]any{"type": "object", "properties": map[string]any{"partitions": map[string]any{"type": "integer", "minimum": 1}}},
						},
					},
				},
				"lambda": map[string]any{
					"type":        "object",
					"description": "Lambda concurrency model.",
					"properties": map[string]any{
						"disabled":            map[string]any{"type": "boolean", "description": "Turn concurrency limits off while the other limits apply."},
						"account_concurrency": map[string]any{"type": "number", "minimum": 0, "description": "Concurrent executions per account and region before scaling; default 1000."},
					},
				},
				"rates": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":            str("Rule name; a base rule's id replaces it."),
							"service":       str("AWS service such as \"ssm\" or \"s3\"."),
							"operations":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Operations sharing the rule; empty means all."},
							"per_second":    map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Sustained rate before scaling."},
							"burst_seconds": map[string]any{"type": "number", "minimum": 0, "description": "Seconds of rate allowed at once; default 1."},
							"per":           map[string]any{"type": "string", "enum": []string{"account", "resource", "prefix"}, "description": "What shares one bucket; prefix is for S3 only."},
							"disabled":      map[string]any{"type": "boolean", "description": "Turn the base rule with this id off."},
						},
					},
				},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				return api.SetLimits(ctx, raw)
			},
		},
		{
			Name:        "stratus_limits_status",
			Description: "Report the active limits profile: each rule's effective rate, admitted and throttled counts, and the latest throttled requests with request IDs.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.LimitsStatus(ctx)
			},
		},
		{
			Name:        "stratus_limits_clear",
			Description: "Turn limits off and return the final status of the profile.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.ClearLimits(ctx)
			},
		},
		{
			Name: "stratus_sigv4_set",
			Description: "Turn on strict signature mode so Stratus verifies every AWS request's SigV4 signature (Authorization header or presigned URL, S3 payload hashes and aws-chunked uploads, expiry, 15-minute clock skew, session tokens) the way AWS does. " +
				"strict refuses bad requests with AWS's own codes (SignatureDoesNotMatch, InvalidSignatureException, IncompleteSignature, InvalidClientTokenId, InvalidAccessKeyId, RequestExpired, RequestTimeTooSkewed); audit lets them through but records them; off accepts anything (the default). " +
				"Known credentials: IAM user keys, STS session credentials, the root keys \"test\" or a twelve-digit account ID with secret \"test\", and keys in STRATUS_SIGV4_CREDENTIALS. Use it to catch request-signing bugs before real AWS does.",
			InputSchema: schema(map[string]any{
				"mode": map[string]any{"type": "string", "enum": []string{"strict", "audit", "off"}, "description": "Default strict."},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct {
					Mode string `json:"mode"`
				}
				if len(raw) > 0 {
					if err := json.Unmarshal(raw, &in); err != nil {
						return nil, err
					}
				}
				if in.Mode == "" {
					in.Mode = "strict"
				}
				return api.SetSigV4(ctx, in.Mode)
			},
		},
		{
			Name: "stratus_sigv4_status",
			Description: "Report signature verification: mode, verified/failed/skipped counts, failures by AWS error code, and the latest failures with the canonical request and string to sign Stratus expected. " +
				"Diff canonical_request against what the client's signer built; the first differing line is the signing bug.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.SigV4Status(ctx)
			},
		},
		{
			Name: "stratus_load_start",
			Description: "Start recording a load run before driving traffic (for example with k6). Stratus counts every AWS request per second and per operation: " +
				"ok, throttled, failed by chaos, failed for real, and p50/p95/p99 latency. limits (a stratus_limits_set profile) and chaos (a stratus_chaos_start experiment) " +
				"are applied for the run and undone when it stops. One run records at a time.",
			InputSchema: schema(map[string]any{
				"name":   str("Label for the run, such as the load script."),
				"limits": map[string]any{"type": "object", "description": "Limits profile for the run, as stratus_limits_set takes it."},
				"chaos":  map[string]any{"type": "object", "description": "Chaos experiment for the run, as stratus_chaos_start takes it."},
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				return api.StartRun(ctx, raw)
			},
		},
		{
			Name:        "stratus_load_status",
			Description: "Report a load run: totals, a per-second timeline and the busiest operations, with live limits and chaos counters. id defaults to the current run; latest is the last finished one.",
			InputSchema: schema(map[string]any{"id": str("Run ID, current or latest.")}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if in.ID == "" {
					in.ID = "current"
				}
				return api.GetRun(ctx, in.ID)
			},
		},
		{
			Name: "stratus_load_report",
			Description: "Analyse a load run's traffic: hot DynamoDB partitions, chatty calls with a batch API, config reads that should be cached, " +
				"missing or exhausted retries, retry storms and throttled writes with no queue in front. Returns findings ranked by severity, " +
				"each with what to do, plus the run's totals. id defaults to the latest run.",
			InputSchema: schema(map[string]any{"id": str("Run ID, current or latest.")}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if in.ID == "" {
					in.ID = "latest"
				}
				out, err := api.GetRun(ctx, in.ID)
				if err != nil {
					return nil, err
				}
				return loadReport(out)
			},
		},
		{
			Name:        "stratus_load_stop",
			Description: "Stop the current load run, clear the limits and chaos it set up, and return its final summary.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.StopRun(ctx, "current")
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
			Name: "stratus_clock_status",
			Description: "Show the emulator clock: its current time, real time, how far ahead it runs and whether it is frozen. " +
				"Every Stratus service reads time from this clock; Lambda containers and database engines keep real time.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if _, err := decode[struct{}](raw); err != nil {
					return nil, err
				}
				return api.ClockStatus(ctx)
			},
		},
		{
			Name: "stratus_clock_advance",
			Description: "Move the emulator clock forward instead of sleeping, then fire every EventBridge schedule that became due before returning. " +
				"Use it to cross an SQS visibility timeout or delay, a CloudWatch alarm's evaluation period, an S3 Object Lock retention date or an hourly schedule in one call. " +
				"The clock never moves backwards; a full reset or a snapshot restore puts it back. Returns the new time and what fired.",
			InputSchema: schema(map[string]any{
				"by": str("How far to advance, as a duration such as \"30m\" or \"36h\"."),
				"to": str("Advance to this RFC 3339 time instead; must be ahead of the clock."),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					By string `json:"by"`
					To string `json:"to"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return api.ClockAdvance(ctx, a.By, a.To)
			},
		},
		{
			Name:        "stratus_clock_freeze",
			Description: "Hold the emulator clock where it stands, so every timestamp Stratus records is the same until you advance or resume it. Useful for snapshot-style assertions.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if _, err := decode[struct{}](raw); err != nil {
					return nil, err
				}
				return api.ClockFreeze(ctx)
			},
		},
		{
			Name:        "stratus_clock_resume",
			Description: "Let a frozen emulator clock run again from where it was held.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if _, err := decode[struct{}](raw); err != nil {
					return nil, err
				}
				return api.ClockResume(ctx)
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
		{
			Name: "stratus_dns_resolve",
			Description: "Resolve a DNS name against the emulated Route 53 hosted zones, including the zones and records Cloud Map creates " +
				"for registered instances. Follows CNAMEs and alias records; found=false means NXDOMAIN or no records of that type.",
			InputSchema: schema(map[string]any{
				"name": map[string]any{"type": "string", "description": "DNS name, e.g. api.corp.local"},
				"type": map[string]any{"type": "string", "description": "Record type: A (default), AAAA, CNAME, SRV, TXT, MX, NS, SOA..."},
			}, "name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var a struct {
					Name string `json:"name"`
					Type string `json:"type"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				return api.DNSResolve(ctx, a.Name, a.Type)
			},
		},
		{
			Name: "stratus_coverage",
			Description: "List which AWS operations of a service Stratus implements and which it answers with 501 NotImplemented, " +
				"from the catalogue generated from AWS's service models. Check here before writing code or tests against an operation. " +
				"Without a service, lists every emulated service with its counts.",
			InputSchema: schema(map[string]any{
				"service":   str("Stratus service name, e.g. \"sqs\", \"dynamodb\", \"cognitoidp\", \"monitoring\" (CloudWatch)."),
				"operation": str("Optional AWS operation name, e.g. \"SendMessage\"; returns just that operation's support."),
			}),
			Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Service   string `json:"service"`
					Operation string `json:"operation"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return coverage(a.Service, a.Operation)
			},
		},
		{
			Name: "stratus_check",
			Description: "Scan a codebase's source (boto3, AWS SDK for JavaScript v3, AWS SDK for Go v2) for the AWS operations it calls " +
				"and report which Stratus does not implement, with file and line, and which services it does not emulate. " +
				"Reads files only; nothing runs. Use it before running a project's tests against Stratus.",
			InputSchema: schema(map[string]any{
				"path": str("Directory to scan; defaults to the current directory."),
			}),
			Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					Path string `json:"path"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.Path == "" {
					a.Path = "."
				}
				return check.Run(a.Path)
			},
		},
	}
}

// coverage answers stratus_coverage from the embedded catalogue.
func coverage(service, operation string) (any, error) {
	if service == "" {
		type row struct {
			Service        string `json:"service"`
			Implemented    int    `json:"implemented"`
			NotImplemented int    `json:"not_implemented"`
		}
		rows := []row{}
		for _, name := range awsmodel.Services() {
			c, err := awsmodel.Coverage(name)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row{name, len(c.Implemented), len(c.NotImplemented)})
		}
		return map[string]any{"services": rows}, nil
	}
	if _, err := awsmodel.Load(service); err != nil {
		return nil, fmt.Errorf("unknown service %q: Stratus emulates %v", service, awsmodel.Services())
	}
	if operation != "" {
		return map[string]any{"service": service, "operation": operation, "support": awsmodel.Lookup(service, operation)}, nil
	}
	return awsmodel.Coverage(service)
}

// loadReport trims a run summary to what an agent needs to act on: the
// findings and totals, without the per-second timeline or operations.
func loadReport(raw json.RawMessage) (any, error) {
	var s map[string]json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, k := range []string{"id", "name", "status", "duration_seconds", "totals", "findings"} {
		if v, ok := s[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
