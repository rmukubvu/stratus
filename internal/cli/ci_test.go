package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/ci"
)

type fakeCIRunner struct {
	opts ci.Options
	code int
	err  error
	ran  bool
}

func (f *fakeCIRunner) Run(_ context.Context, opts ci.Options) (int, error) {
	f.ran, f.opts = true, opts
	return f.code, f.err
}

func TestRunCIParsesFlagsAndCommand(t *testing.T) {
	r := &fakeCIRunner{code: 4}
	var stderr bytes.Buffer
	code := RunCI(context.Background(), []string{
		"--seed", "snaps/seeded", "--keep-data", "--log-level", "debug", "--ready-timeout", "5s",
		"--", "go", "test", "-run", "X", "./...",
	}, r, &stderr)

	if code != 4 {
		t.Fatalf("exit code %d, want 4 (the command's)", code)
	}
	want := ci.Options{
		Command:      []string{"go", "test", "-run", "X", "./..."},
		Seed:         "snaps/seeded",
		KeepData:     true,
		LogLevel:     "debug",
		ReadyTimeout: 5 * time.Second,
		Stderr:       &stderr,
	}
	if !reflect.DeepEqual(r.opts, want) {
		t.Fatalf("options %+v, want %+v", r.opts, want)
	}
}

func TestRunCIDefaults(t *testing.T) {
	r := &fakeCIRunner{}
	RunCI(context.Background(), []string{"--", "make", "test"}, r, &bytes.Buffer{})
	if r.opts.LogLevel != "warn" || r.opts.ReadyTimeout != time.Minute || r.opts.KeepData || r.opts.Seed != "" {
		t.Fatalf("defaults: %+v", r.opts)
	}
}

func TestRunCIRequiresCommand(t *testing.T) {
	r := &fakeCIRunner{}
	var stderr bytes.Buffer
	if code := RunCI(context.Background(), []string{"--seed", "x"}, r, &stderr); code != 2 || r.ran {
		t.Fatalf("code=%d ran=%v", code, r.ran)
	}
	if !strings.Contains(stderr.String(), "usage: stratus ci") {
		t.Fatalf("missing usage: %q", stderr.String())
	}
}

func TestRunCIPrintsRunnerErrors(t *testing.T) {
	r := &fakeCIRunner{code: ci.ExitRunnerFailed, err: errors.New("start stratus: port in use")}
	var stderr bytes.Buffer
	if code := RunCI(context.Background(), []string{"--", "true"}, r, &stderr); code != ci.ExitRunnerFailed {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "port in use") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
