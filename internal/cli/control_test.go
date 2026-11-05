package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeControl struct {
	calls    []string
	err      error
	endpoint string
}

func (f *fakeControl) record(call string) (json.RawMessage, error) {
	f.calls = append(f.calls, call)
	return json.RawMessage(`{"ok":true}`), f.err
}

func (f *fakeControl) Reset(_ context.Context, services ...string) (json.RawMessage, error) {
	return f.record("reset " + strings.Join(services, ","))
}
func (f *fakeControl) CreateSnapshot(_ context.Context, name string) (json.RawMessage, error) {
	return f.record("save " + name)
}
func (f *fakeControl) ListSnapshots(context.Context) (json.RawMessage, error) {
	return f.record("list")
}
func (f *fakeControl) RestoreSnapshot(_ context.Context, name string) (json.RawMessage, error) {
	return f.record("restore " + name)
}
func (f *fakeControl) DeleteSnapshot(_ context.Context, name string) error {
	_, err := f.record("delete " + name)
	return err
}
func (f *fakeControl) StartChaos(_ context.Context, spec json.RawMessage) (json.RawMessage, error) {
	return f.record("chaos-start " + string(spec))
}
func (f *fakeControl) ChaosReport(context.Context) (json.RawMessage, error) {
	return f.record("chaos-status")
}
func (f *fakeControl) StopChaos(context.Context) (json.RawMessage, error) {
	return f.record("chaos-stop")
}
func (f *fakeControl) WaitReady(context.Context, time.Duration) error {
	_, err := f.record("wait")
	return err
}

func run(t *testing.T, fake *fakeControl, args ...string) (int, string, string) {
	t.Helper()
	mode, rest := ParseMode(args)
	var stdout, stderr bytes.Buffer
	code := RunControl(context.Background(), mode, rest, func(endpoint string) (ControlClient, error) {
		fake.endpoint = endpoint
		return fake, nil
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestParseModeRecognisesControlCommands(t *testing.T) {
	for _, m := range []Mode{ModeWait, ModeReset, ModeSnapshot, ModeChaos} {
		got, rest := ParseMode([]string{string(m), "x"})
		if got != m || !got.IsControl() || !reflect.DeepEqual(rest, []string{"x"}) {
			t.Errorf("ParseMode(%s) = %s %v", m, got, rest)
		}
	}
	if ModeServe.IsControl() || ModeDev.IsControl() {
		t.Error("server modes must not be control modes")
	}
}

func TestControlCommandsMapToClientCalls(t *testing.T) {
	cases := []struct {
		args []string
		call string
	}{
		{[]string{"wait"}, "wait"},
		{[]string{"reset"}, "reset "},
		{[]string{"reset", "s3", "sqs"}, "reset s3,sqs"},
		{[]string{"snapshot", "save"}, "save "},
		{[]string{"snapshot", "save", "baseline"}, "save baseline"},
		{[]string{"snapshot", "restore", "baseline"}, "restore baseline"},
		{[]string{"snapshot", "list"}, "list"},
		{[]string{"snapshot", "delete", "baseline"}, "delete baseline"},
	}
	for _, tc := range cases {
		fake := &fakeControl{}
		code, _, stderr := run(t, fake, tc.args...)
		if code != 0 || len(fake.calls) != 1 || fake.calls[0] != tc.call {
			t.Errorf("%v: code=%d calls=%v stderr=%s", tc.args, code, fake.calls, stderr)
		}
	}
}

func TestControlCommandPrintsJSONOnSuccess(t *testing.T) {
	code, stdout, _ := run(t, &fakeControl{}, "snapshot", "list")
	if code != 0 || strings.TrimSpace(stdout) != `{"ok":true}` {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
}

func TestControlCommandUsesEndpointFlagAndEnv(t *testing.T) {
	fake := &fakeControl{}
	run(t, fake, "reset", "--endpoint", "http://ci:9999")
	if fake.endpoint != "http://ci:9999" {
		t.Fatalf("endpoint flag ignored: %q", fake.endpoint)
	}

	t.Setenv(EndpointEnv, "http://from-env:1")
	run(t, fake, "reset")
	if fake.endpoint != "http://from-env:1" {
		t.Fatalf("endpoint env ignored: %q", fake.endpoint)
	}
}

func TestControlCommandReportsErrors(t *testing.T) {
	code, _, stderr := run(t, &fakeControl{err: errors.New("SnapshotNotFound: nope")}, "snapshot", "restore", "x")
	if code != 1 || !strings.Contains(stderr, "SnapshotNotFound") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestSnapshotUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"snapshot"},
		{"snapshot", "restore"},
		{"snapshot", "delete", "a", "b"},
		{"snapshot", "save", "a", "b"},
		{"snapshot", "frobnicate"},
	} {
		fake := &fakeControl{}
		code, _, stderr := run(t, fake, args...)
		if code != 2 || len(fake.calls) != 0 || !strings.Contains(stderr, "usage") {
			t.Errorf("%v: code=%d calls=%v stderr=%q", args, code, fake.calls, stderr)
		}
	}
}

func TestChaosCommands(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "exp.json")
	if err := os.WriteFile(spec, []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		call string
	}{
		{[]string{"chaos", "start", spec}, `chaos-start {"rules":[]}`},
		{[]string{"chaos", "status"}, "chaos-status"},
		{[]string{"chaos", "stop"}, "chaos-stop"},
	}
	for _, tc := range cases {
		fake := &fakeControl{}
		code, _, stderr := run(t, fake, tc.args...)
		if code != 0 || len(fake.calls) != 1 || fake.calls[0] != tc.call {
			t.Errorf("%v: code=%d calls=%v stderr=%s", tc.args, code, fake.calls, stderr)
		}
	}
}

func TestChaosStartRejectsMissingOrInvalidFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	for _, args := range [][]string{
		{"chaos", "start", filepath.Join(t.TempDir(), "missing.json")},
		{"chaos", "start", bad},
	} {
		fake := &fakeControl{}
		code, _, _ := run(t, fake, args...)
		if code != 1 || len(fake.calls) != 0 {
			t.Errorf("%v: code=%d calls=%v", args, code, fake.calls)
		}
	}
	for _, args := range [][]string{{"chaos"}, {"chaos", "start"}, {"chaos", "explode"}} {
		if code, _, _ := run(t, &fakeControl{}, args...); code != 2 {
			t.Errorf("%v: code=%d, want usage error", args, code)
		}
	}
}
