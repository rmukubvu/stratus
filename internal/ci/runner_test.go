package ci

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeServer struct {
	stopped bool
	stopErr error
}

func (f *fakeServer) Endpoint() string { return "http://127.0.0.1:4999" }
func (f *fakeServer) Port() int        { return 4999 }
func (f *fakeServer) Stop() error      { f.stopped = true; return f.stopErr }

type recorder struct {
	events  []string
	server  *fakeServer
	cfg     ServerConfig
	env     []string
	command []string
}

func (r *recorder) runner(startErr, readyErr, execErr error, exitCode int) Runner {
	r.server = &fakeServer{}
	return Runner{
		Start: func(_ context.Context, cfg ServerConfig) (Server, error) {
			r.events = append(r.events, "start")
			r.cfg = cfg
			if startErr != nil {
				return nil, startErr
			}
			return r.server, nil
		},
		Ready: func(context.Context, string) error {
			r.events = append(r.events, "ready")
			return readyErr
		},
		Execute: func(_ context.Context, command, env []string) (int, error) {
			r.events = append(r.events, "exec")
			r.command, r.env = command, env
			return exitCode, execErr
		},
		TempDir: func() (string, error) { r.events = append(r.events, "mkdir"); return "/tmp/run-1", nil },
		Remove:  func(string) error { r.events = append(r.events, "rmdir"); return nil },
	}
}

func TestRunPassesThroughCommandExitCodeAndCleansUp(t *testing.T) {
	r := &recorder{}
	code, err := r.runner(nil, nil, nil, 3).Run(context.Background(), Options{
		Command: []string{"go", "test", "./..."}, Seed: "/snaps/seeded", Env: []string{"PATH=/bin"},
	})
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if want := []string{"mkdir", "start", "ready", "exec", "rmdir"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if !r.server.stopped {
		t.Fatal("server not stopped")
	}
	if r.cfg.DataDir != "/tmp/run-1" || r.cfg.Seed != "/snaps/seeded" {
		t.Fatalf("server config: %+v", r.cfg)
	}
	if !reflect.DeepEqual(r.command, []string{"go", "test", "./..."}) {
		t.Fatalf("command: %v", r.command)
	}
	if !contains(r.env, "AWS_ENDPOINT_URL=http://127.0.0.1:4999") || !contains(r.env, "PATH=/bin") {
		t.Fatalf("env: %v", r.env)
	}
}

func TestRunCleansUpWhenServerNeverBecomesReady(t *testing.T) {
	r := &recorder{}
	code, err := r.runner(nil, errors.New("not ready"), nil, 0).Run(context.Background(), Options{Command: []string{"true"}})
	if err == nil || code != ExitRunnerFailed {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !r.server.stopped || r.events[len(r.events)-1] != "rmdir" || contains(r.events, "exec") {
		t.Fatalf("events %v stopped=%v", r.events, r.server.stopped)
	}
}

func TestRunRemovesDataDirWhenStartFails(t *testing.T) {
	r := &recorder{}
	code, err := r.runner(errors.New("port in use"), nil, nil, 0).Run(context.Background(), Options{Command: []string{"true"}})
	if err == nil || code != ExitRunnerFailed || !reflect.DeepEqual(r.events, []string{"mkdir", "start", "rmdir"}) {
		t.Fatalf("code=%d err=%v events=%v", code, err, r.events)
	}
}

func TestRunReportsExecFailureAsRunnerFailure(t *testing.T) {
	r := &recorder{}
	code, err := r.runner(nil, nil, errors.New("executable not found"), 0).Run(context.Background(), Options{Command: []string{"nope"}})
	if err == nil || code != ExitRunnerFailed || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !r.server.stopped {
		t.Fatal("server not stopped")
	}
}

func TestRunKeepsDataDirOnRequest(t *testing.T) {
	r := &recorder{}
	var stderr strings.Builder
	if _, err := r.runner(nil, nil, nil, 0).Run(context.Background(), Options{Command: []string{"true"}, KeepData: true, Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	if contains(r.events, "rmdir") || !strings.Contains(stderr.String(), "/tmp/run-1") {
		t.Fatalf("events=%v stderr=%q", r.events, stderr.String())
	}
}

func TestRunReportsStopFailureAfterSuccessfulCommand(t *testing.T) {
	r := &recorder{}
	run := r.runner(nil, nil, nil, 0)
	r.server.stopErr = errors.New("stuck")
	code, err := run.Run(context.Background(), Options{Command: []string{"true"}})
	if err == nil || code != ExitRunnerFailed {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestRunRequiresCommand(t *testing.T) {
	r := &recorder{}
	code, err := r.runner(nil, nil, nil, 0).Run(context.Background(), Options{})
	if err == nil || code != ExitRunnerFailed || len(r.events) != 0 {
		t.Fatalf("code=%d err=%v events=%v", code, err, r.events)
	}
}

func TestCommandEnvForcesEndpointsAndOnlyDefaultsCredentials(t *testing.T) {
	env := CommandEnv([]string{
		"PATH=/bin",
		"AWS_ENDPOINT_URL=https://real.aws",
		"AWS_REGION=eu-west-1",
		"AWS_ACCESS_KEY_ID=mine",
	}, "http://127.0.0.1:5000", 5000)

	for _, want := range []string{
		"PATH=/bin",
		"AWS_ENDPOINT_URL=http://127.0.0.1:5000",
		"STRATUS_ENDPOINT=http://127.0.0.1:5000",
		"STRATUS_DOCKER_ENDPOINT=http://host.docker.internal:5000",
		"STRATUS_PORT=5000",
		"AWS_DISABLE_HOST_PREFIX_INJECTION=true",
		"AWS_REGION=eu-west-1",
		"AWS_ACCESS_KEY_ID=mine",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_DEFAULT_REGION=us-east-1",
	} {
		if !contains(env, want) {
			t.Errorf("missing %s in %v", want, env)
		}
	}
	for _, unwanted := range []string{"AWS_ENDPOINT_URL=https://real.aws", "AWS_REGION=us-east-1", "AWS_ACCESS_KEY_ID=test"} {
		if contains(env, unwanted) {
			t.Errorf("unexpected %s", unwanted)
		}
	}
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
