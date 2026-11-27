package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

// adviseTools guide an architecture decision: which AWS services fit a
// workload, ending in an ADR. The agent holds the conversation; Stratus
// owns the questions, the rules and the quotas behind each verdict.
func adviseTools(api ControlAPI) []Tool {
	idSchema := func() map[string]any {
		return schema(map[string]any{"id": str("Decision ID, e.g. adr-3f9c2a1b7d04.")}, "id")
	}
	requireID := func(raw json.RawMessage) (string, error) {
		a, err := decode[struct {
			ID string `json:"id"`
		}](raw)
		if err != nil {
			return "", err
		}
		if a.ID == "" {
			return "", errors.New("invalid arguments: id is required")
		}
		return a.ID, nil
	}
	answersSchema := map[string]any{
		"type":                 "object",
		"description":          "Answers keyed by question ID, e.g. {\"ordering\":\"per_key\",\"peak_rate\":2000,\"replay\":true}.",
		"additionalProperties": true,
	}
	return []Tool{
		{
			Name: "stratus_advise_families",
			Description: "List the architecture decisions Stratus can guide (for example messaging: SQS, SQS FIFO, SNS fan-out, EventBridge, " +
				"Kinesis, MSK), each with its questions and candidate options. Use it when someone is choosing AWS services for a workload.",
			InputSchema: schema(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return api.AdviseFamilies(ctx)
			},
		},
		{
			Name: "stratus_advise_start",
			Description: "Start an architecture decision. Returns the decision with its pending questions, each with a reason and a default. " +
				"Ask the person each pending question in your own words rather than guessing; unanswered questions use their defaults " +
				"and the ADR marks them as assumed. Answers already known can be passed here.",
			InputSchema: schema(map[string]any{
				"family":  str("Decision family, e.g. \"messaging\"."),
				"title":   str("A short name for the decision, e.g. \"Order events\"."),
				"answers": answersSchema,
			}, "family"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				if _, err := decode[struct {
					Family  string         `json:"family"`
					Title   string         `json:"title"`
					Answers map[string]any `json:"answers"`
				}](raw); err != nil {
					return nil, err
				}
				return api.AdviseStart(ctx, raw)
			},
		},
		{
			Name:        "stratus_advise_answer",
			Description: "Record the person's answers to a decision's questions. Changing an answer drops any earlier assessment, so assess again afterwards.",
			InputSchema: schema(map[string]any{"id": str("Decision ID."), "answers": answersSchema}, "id", "answers"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID      string         `json:"id"`
					Answers map[string]any `json:"answers"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.ID == "" || len(a.Answers) == 0 {
					return nil, errors.New("invalid arguments: id and answers are required")
				}
				return api.AdviseAnswer(ctx, a.ID, a.Answers)
			},
		},
		{
			Name: "stratus_advise_assess",
			Description: "Judge every option against the answers and rank them: fits, fits with caveats, or ruled out, each with reasons " +
				"citing AWS's documented quotas, a fit score and an ops-overhead score. Show the person the ranking and the reasons before deciding.",
			InputSchema: idSchema(),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				id, err := requireID(raw)
				if err != nil {
					return nil, err
				}
				return api.AdviseAssess(ctx, id)
			},
		},
		{
			Name: "stratus_advise_decide",
			Description: "Record the option the person chose, with their reason. Only an assessed option that was not ruled out can be chosen. " +
				"Let the person make the choice; do not decide for them.",
			InputSchema: schema(map[string]any{
				"id":        str("Decision ID."),
				"option":    str("Option ID from the assessment, e.g. \"kinesis\"."),
				"rationale": str("Why, in the person's words; it goes into the ADR."),
			}, "id", "option"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID        string `json:"id"`
					Option    string `json:"option"`
					Rationale string `json:"rationale"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.ID == "" || a.Option == "" {
					return nil, errors.New("invalid arguments: id and option are required")
				}
				return api.AdviseDecide(ctx, a.ID, a.Option, a.Rationale)
			},
		},
		{
			Name: "stratus_advise_adr",
			Description: "Render a decision as a Markdown ADR (MADR layout): requirements stated or assumed, options with verdicts and cited reasons, " +
				"the decision and its consequences, and ops overhead. Write it to the repository, for example docs/adr/NNNN-title.md.",
			InputSchema: idSchema(),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				id, err := requireID(raw)
				if err != nil {
					return nil, err
				}
				md, err := api.AdviseADR(ctx, id)
				if err != nil {
					return nil, err
				}
				return map[string]string{"id": id, "markdown": md}, nil
			},
		},
		{
			Name:        "stratus_advise_get",
			Description: "Show a decision: its answers, pending questions, ranking, recommended option and what to do next. Without an id, lists decisions.",
			InputSchema: schema(map[string]any{"id": str("Decision ID; omit to list decisions.")}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				a, err := decode[struct {
					ID string `json:"id"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if a.ID == "" {
					return api.AdviseList(ctx)
				}
				return api.AdviseGet(ctx, a.ID)
			},
		},
	}
}
