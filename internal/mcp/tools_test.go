package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

type fakeAPI struct{ calls []string }

func (f *fakeAPI) rec(c string) (json.RawMessage, error) {
	f.calls = append(f.calls, c)
	return json.RawMessage(`{"ok":true}`), nil
}
func (f *fakeAPI) Endpoint() string                                { return "http://127.0.0.1:5555" }
func (f *fakeAPI) Health(context.Context) (json.RawMessage, error) { return f.rec("health") }
func (f *fakeAPI) Reset(_ context.Context, s ...string) (json.RawMessage, error) {
	return f.rec("reset " + strings.Join(s, ","))
}
func (f *fakeAPI) CreateSnapshot(_ context.Context, n string) (json.RawMessage, error) {
	return f.rec("save " + n)
}
func (f *fakeAPI) ListSnapshots(context.Context) (json.RawMessage, error) { return f.rec("list") }
func (f *fakeAPI) RestoreSnapshot(_ context.Context, n string) (json.RawMessage, error) {
	return f.rec("restore " + n)
}
func (f *fakeAPI) DeleteSnapshot(_ context.Context, n string) error {
	_, err := f.rec("delete " + n)
	return err
}
func (f *fakeAPI) StartChaos(_ context.Context, spec json.RawMessage) (json.RawMessage, error) {
	return f.rec("chaos-start " + string(spec))
}
func (f *fakeAPI) ChaosReport(context.Context) (json.RawMessage, error) { return f.rec("chaos-status") }
func (f *fakeAPI) StopChaos(context.Context) (json.RawMessage, error)   { return f.rec("chaos-stop") }
func (f *fakeAPI) Diagnose(_ context.Context, q url.Values) (json.RawMessage, error) {
	return f.rec("diagnose " + q.Encode())
}

func call(t *testing.T, api *fakeAPI, name, args string) *toolResult {
	t.Helper()
	s, err := NewServer("stratus", "test", Instructions, Tools(api)...)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	res, rpcErr := s.callTool(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("%s: rpc error %v", name, rpcErr)
	}
	return res
}

func TestToolsMapToControlCalls(t *testing.T) {
	cases := []struct{ tool, args, want string }{
		{"stratus_status", `{}`, "health"},
		{"stratus_reset", `{}`, "reset "},
		{"stratus_reset", `{"services":["s3","sqs"]}`, "reset s3,sqs"},
		{"stratus_snapshot_save", `{"name":"seeded"}`, "save seeded"},
		{"stratus_snapshot_save", `{}`, "save "},
		{"stratus_snapshot_restore", `{"name":"seeded"}`, "restore seeded"},
		{"stratus_snapshot_list", `{}`, "list"},
		{"stratus_snapshot_delete", `{"name":"seeded"}`, "delete seeded"},
		{"stratus_chaos_start", `{"rules":[{"service":"s3","action":"drop"}]}`, `chaos-start {"rules":[{"service":"s3","action":"drop"}]}`},
		{"stratus_chaos_status", `{}`, "chaos-status"},
		{"stratus_chaos_stop", `{}`, "chaos-stop"},
		{"stratus_diagnose", `{"since":"5m","service":"sqs","exclude_injected":true,"max_groups":3}`, "diagnose exclude_injected=true&max_groups=3&service=sqs&since=5m"},
	}
	for _, tc := range cases {
		api := &fakeAPI{}
		res := call(t, api, tc.tool, tc.args)
		if res.IsError || len(api.calls) != 1 || api.calls[0] != tc.want {
			t.Errorf("%s %s: isError=%v calls=%v", tc.tool, tc.args, res.IsError, api.calls)
		}
	}
}

func TestToolArgumentErrorsAreReportedToTheModel(t *testing.T) {
	for _, tc := range []struct{ tool, args string }{
		{"stratus_snapshot_restore", `{}`},
		{"stratus_snapshot_delete", `{"name":""}`},
		{"stratus_reset", `{"services":"s3"}`},
		{"stratus_diagnose", `{"unknown":1}`},
	} {
		api := &fakeAPI{}
		res := call(t, api, tc.tool, tc.args)
		if !res.IsError || len(api.calls) != 0 || !strings.Contains(res.Content[0].Text, "invalid arguments") {
			t.Errorf("%s %s: %+v calls=%v", tc.tool, tc.args, res, api.calls)
		}
	}
}

func TestConnectionInfo(t *testing.T) {
	res := call(t, &fakeAPI{}, "stratus_connection_info", `{}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, `"AWS_ENDPOINT_URL": "http://127.0.0.1:5555"`) {
		t.Fatalf("connection info: %+v", res)
	}
}

func TestEveryToolHasDescriptionAndObjectSchema(t *testing.T) {
	for _, tool := range Tools(&fakeAPI{}) {
		if !strings.HasPrefix(tool.Name, "stratus_") || len(tool.Description) < 20 {
			t.Errorf("%s: weak name or description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s: schema type %v", tool.Name, tool.InputSchema["type"])
		}
	}
}
