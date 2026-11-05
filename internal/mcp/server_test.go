package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func echoServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer("test", "1.0", "be nice",
		Tool{
			Name:        "echo",
			Description: "echoes",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(_ context.Context, args json.RawMessage) (any, error) {
				return json.RawMessage(args), nil
			},
		},
		Tool{
			Name: "fail",
			Handler: func(context.Context, json.RawMessage) (any, error) {
				return nil, errors.New("SnapshotNotFound: nope (hint: list them)")
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// exchange sends lines and returns decoded responses in order.
func exchange(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		m := map[string]any{}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad response %q: %v", sc.Text(), err)
		}
		resps = append(resps, m)
	}
	return resps
}

func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	return r
}

func errCode(resp map[string]any) float64 {
	e, _ := resp["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	resps := exchange(t, echoServer(t),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
	)
	r := resultOf(t, resps[0])
	if r["protocolVersion"] != "2025-03-26" || r["instructions"] != "be nice" {
		t.Fatalf("init: %v", r)
	}
	if info := r["serverInfo"].(map[string]any); info["name"] != "test" || info["version"] != "1.0" {
		t.Fatalf("server info: %v", info)
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("tools capability missing: %v", r)
	}
	if v := resultOf(t, resps[1])["protocolVersion"]; v != SupportedVersions[0] {
		t.Fatalf("unknown version should fall back to latest, got %v", v)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	resps := exchange(t, echoServer(t),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"p","method":"ping"}`,
	)
	if len(resps) != 1 || resps[0]["id"] != "p" {
		t.Fatalf("responses: %v", resps)
	}
}

func TestToolsListAndCall(t *testing.T) {
	resps := exchange(t, echoServer(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
	)
	tools := resultOf(t, resps[0])["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools: %v", tools)
	}
	if schema := tools[1].(map[string]any)["inputSchema"].(map[string]any); schema["type"] != "object" {
		t.Fatalf("default schema: %v", schema)
	}

	content := resultOf(t, resps[1])["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || !strings.Contains(content["text"].(string), `"x": 1`) {
		t.Fatalf("echo content: %v", content)
	}

	failed := resultOf(t, resps[2])
	if failed["isError"] != true || !strings.Contains(failed["content"].([]any)[0].(map[string]any)["text"].(string), "SnapshotNotFound") {
		t.Fatalf("tool error should be a result the model can read: %v", failed)
	}

	if errCode(resps[3]) != codeInvalidParams {
		t.Fatalf("unknown tool: %v", resps[3])
	}
}

func TestProtocolErrors(t *testing.T) {
	resps := exchange(t, echoServer(t),
		`{not json`,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		`{"jsonrpc":"1.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`,
	)
	want := []float64{codeParseError, codeMethodNotFound, codeInvalidRequest, codeInvalidParams}
	if len(resps) != len(want) {
		t.Fatalf("responses: %v", resps)
	}
	for i, code := range want {
		if errCode(resps[i]) != code {
			t.Errorf("response %d: got %v, want code %v", i, resps[i], code)
		}
	}
}

func TestNewServerRejectsDuplicateTools(t *testing.T) {
	if _, err := NewServer("x", "1", "", Tool{Name: "a"}, Tool{Name: "a"}); err == nil {
		t.Fatal("expected duplicate error")
	}
}
