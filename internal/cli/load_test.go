package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadCommands(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"load", "start", "--name", "smoke", "--limits", "strict"}, `run-start {"limits":{"profile":"strict"},"name":"smoke"}`},
		{[]string{"load", "start"}, "run-start {}"},
		{[]string{"load", "status"}, "run-get current"},
		{[]string{"load", "status", "run-3"}, "run-get run-3"},
		{[]string{"load", "stop"}, "run-stop current"},
		{[]string{"load", "list"}, "run-list"},
	}
	for _, tc := range cases {
		fake := &fakeControl{}
		if code, _, stderr := run(t, fake, tc.args...); code != 0 {
			t.Fatalf("%v: exit %d (%s)", tc.args, code, stderr)
		}
		if len(fake.calls) != 1 || fake.calls[0] != tc.want {
			t.Errorf("%v: calls %v, want %q", tc.args, fake.calls, tc.want)
		}
	}
	if code, _, _ := run(t, &fakeControl{}, "load"); code != 2 {
		t.Fatalf("bare load: exit %d", code)
	}
	if code, _, _ := run(t, &fakeControl{}, "load", "run", "--name", "x"); code != 2 {
		t.Fatalf("run without a command: exit %d", code)
	}
}

func TestLoadStartReadsAChaosFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chaos.json")
	_ = os.WriteFile(path, []byte(`{"seed":7,"rules":[]}`), 0o600)
	fake := &fakeControl{}
	if code, _, stderr := run(t, fake, "load", "start", "--chaos", path); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if fake.calls[0] != `run-start {"chaos":{"seed":7,"rules":[]}}` {
		t.Fatalf("calls %v", fake.calls)
	}
}

func TestLoadRunWrapsTheCommandAndPassesItsExitCode(t *testing.T) {
	var gotArgv, gotEnv []string
	commandRunner = func(_ context.Context, argv, env []string, _, _ io.Writer) (int, error) {
		gotArgv, gotEnv = argv, env
		return 3, nil
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })

	fake := &fakeControl{}
	code, stdout, _ := run(t, fake, "load", "run", "--limits", "strict", "--", "k6", "run", "load.js")
	if code != 3 {
		t.Fatalf("exit %d, want the command's 3", code)
	}
	if !reflect.DeepEqual(gotArgv, []string{"k6", "run", "load.js"}) {
		t.Fatalf("argv %v", gotArgv)
	}
	if !strings.Contains(strings.Join(gotEnv, " "), "STRATUS_RUN_ID=run-1") {
		t.Fatalf("env %v", gotEnv)
	}
	if len(fake.calls) != 2 || !strings.HasPrefix(fake.calls[0], "run-start") || fake.calls[1] != "run-stop run-1" {
		t.Fatalf("calls %v: the run must stop even when the command fails", fake.calls)
	}
	if !strings.Contains(stdout, "Timeline and full detail") {
		t.Fatalf("report not printed: %q", stdout)
	}

	fake = &fakeControl{}
	if code, stdout, _ = run(t, fake, "load", "run", "--json", "--", "k6"); code != 3 || !strings.Contains(stdout, `"ok":true`) {
		t.Fatalf("--json: exit %d, stdout %q", code, stdout)
	}
}

func TestLoadStopPrintsAReportOrJSON(t *testing.T) {
	fake := &fakeControl{}
	if code, stdout, _ := run(t, fake, "load", "stop", "--json", "run-2"); code != 0 || fake.calls[0] != "run-stop run-2" || !strings.Contains(stdout, `"ok":true`) {
		t.Fatalf("stop --json: exit %d, calls %v, stdout %q", code, fake.calls, stdout)
	}
}

func TestRunReportSummarisesARun(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "run-4", "name": "checkout", "status": "finished", "duration_seconds": 30.04,
		"totals": {"requests": 600, "ok": 500, "throttled": 90, "faults": 6, "errors": 4,
			"p50_ms": 1.5, "p95_ms": 12, "p99_ms": 40.25, "max_ms": 80,
			"per_second": 20, "throttle_rate": 0.15, "error_rate": 0.01},
		"operations": [{"service": "dynamodb", "operation": "PutItem", "requests": 600, "throttled": 90, "faults": 6, "errors": 4, "p95_ms": 12}],
		"limits": {"profile": "strict", "total_throttled": 90},
		"chaos": {"id": "exp-1", "seed": 42, "total_injected": 6}
	}`)
	var out strings.Builder
	if err := writeRunReport(&out, raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"run-4 (checkout) finished after 30.0s",
		"20.0/s", "15%", "stratus diagnose",
		"p50 1.5ms  p95 12ms  p99 40.25ms  max 80ms",
		"dynamodb PutItem", "strict profile, 90 throttled", "exp-1 (seed 42), 6 faults injected",
		"stratus load status run-4",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if err := writeRunReport(&out, json.RawMessage(`not json`)); err == nil {
		t.Fatal("a broken summary was reported")
	}
}
