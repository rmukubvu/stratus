package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/ci"
)

// TestMCPEndToEnd starts a real Stratus through `stratus mcp --start` and
// drives it as an MCP client would.
func TestMCPEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var dataDir string
	deps := MCPDeps{
		Start: ci.StartInProcess,
		Ready: ci.WaitReady,
		TempDir: func() (string, error) {
			d, err := os.MkdirTemp(t.TempDir(), "mcp-*")
			dataDir = d
			return d, err
		},
		Remove:  os.RemoveAll,
		Version: "test",
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- RunMCP(ctx, []string{"--start"}, deps, inR, outW, io.Discard)
		outW.Close()
	}()

	responses := bufio.NewScanner(outR)
	id := 0
	rpc := func(method string, params any) map[string]any {
		t.Helper()
		id++
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(msg, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !responses.Scan() {
			t.Fatalf("no response to %s: %v", method, responses.Err())
		}
		out := map[string]any{}
		if err := json.Unmarshal(responses.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", responses.Text(), err)
		}
		return out["result"].(map[string]any)
	}
	toolText := func(name string, args any) (string, bool) {
		t.Helper()
		r := rpc("tools/call", map[string]any{"name": name, "arguments": args})
		isErr, _ := r["isError"].(bool)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), isErr
	}

	if v := rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("protocol version %v", v)
	}
	if n := len(rpc("tools/list", nil)["tools"].([]any)); n < 10 {
		t.Fatalf("only %d tools", n)
	}

	text, isErr := toolText("stratus_status", map[string]any{})
	if isErr || !strings.Contains(text, `"ready": true`) {
		t.Fatalf("status: %s", text)
	}
	text, isErr = toolText("stratus_snapshot_save", map[string]any{"name": "seeded"})
	if isErr || !strings.Contains(text, `"id": "seeded"`) {
		t.Fatalf("save: %s", text)
	}
	text, isErr = toolText("stratus_snapshot_restore", map[string]any{"name": "missing"})
	if !isErr || !strings.Contains(text, "SnapshotNotFound") || !strings.Contains(text, "hint") {
		t.Fatalf("restore missing should be a readable tool error: %s", text)
	}
	text, isErr = toolText("stratus_diagnose", map[string]any{})
	if isErr || !strings.Contains(text, "No failed requests") {
		t.Fatalf("diagnose: %s", text)
	}

	inW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-ctx.Done():
		t.Fatal("mcp server did not exit after stdin closed")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("temporary data dir %s not removed", dataDir)
	}
}

func TestMCPRejectsPositionalArgs(t *testing.T) {
	if code := RunMCP(context.Background(), []string{"extra"}, MCPDeps{}, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Fatalf("code %d", code)
	}
}
