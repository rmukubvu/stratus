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
}

// Instructions are sent to the client at initialization.
const Instructions = `Stratus is a local AWS emulator. Point AWS SDKs and the AWS CLI at it with the
environment from stratus_connection_info. Typical loop: stratus_status to check it is
ready, stratus_snapshot_save after seeding, run tests, stratus_diagnose when something
fails (it says whether a failure is your bug, an injected chaos fault, or an emulator
gap), stratus_snapshot_restore to get back to the seeded state. Use stratus_chaos_start
to inject seeded, reproducible faults.`

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
			Description: "Capture all current emulated state under a name, so it can be restored later. Fast and cheap; take one after seeding test data.",
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
			Description: "Start a seeded chaos experiment that injects faults into AWS calls (replaces any running one). " +
				"Each rule has service (or \"*\"), optional operation and resource substring, optional probability (0-1) and max_hits, and an action: " +
				"throttle, internal_error, unavailable, error (with error.status_code and error.code), latency (with latency.delay_ms and optional jitter_ms), drop, or cold_start (lambda only). " +
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
							"id":          str("Rule name shown in reports."),
							"service":     str("AWS service such as \"dynamodb\", or \"*\"."),
							"operation":   str("API operation such as \"PutItem\"."),
							"resource":    str("Substring of the path, bucket or function name."),
							"probability": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
							"max_hits":    map[string]any{"type": "integer", "minimum": 0},
							"action":      map[string]any{"type": "string", "enum": []string{"throttle", "internal_error", "unavailable", "error", "latency", "drop", "cold_start"}},
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
	}
}
