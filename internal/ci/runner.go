// Package ci runs a command against a private, throwaway Stratus instance:
// start on a free port with a fresh data directory (optionally seeded from a
// snapshot), wait until ready, run the command with AWS endpoint variables
// set, then tear everything down, including Lambda containers, whatever the
// command's outcome.
package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// ExitRunnerFailed is returned when Stratus itself could not run the
// command, so callers can tell infrastructure failures from test failures.
const ExitRunnerFailed = 125

// Options configures one run.
type Options struct {
	// Command and its arguments. Required.
	Command []string
	// Seed is an optional snapshot directory to start from.
	Seed string
	// KeepData leaves the data directory on disk for debugging.
	KeepData bool
	// LogLevel for the Stratus server.
	LogLevel string
	// ReadyTimeout bounds how long to wait for Stratus to become ready.
	ReadyTimeout time.Duration
	// Env is the base environment for the command; nil means os.Environ().
	Env []string
	// Stderr receives runner messages.
	Stderr io.Writer
}

// ServerConfig is what a ServerStarter needs to start Stratus.
type ServerConfig struct {
	DataDir  string
	Seed     string
	LogLevel string
}

// Server is a running Stratus instance.
type Server interface {
	// Endpoint is the base URL, e.g. http://127.0.0.1:53211.
	Endpoint() string
	// Port the server listens on.
	Port() int
	// Stop shuts the server down and releases its resources.
	Stop() error
}

// Ports the Runner depends on.
type (
	ServerStarter func(ctx context.Context, cfg ServerConfig) (Server, error)
	ReadyWaiter   func(ctx context.Context, endpoint string) error
	CommandRunner func(ctx context.Context, command []string, env []string) (exitCode int, err error)
)

// Runner orchestrates a run. Its fields are injected so it can be tested
// without starting real servers or processes.
type Runner struct {
	Start   ServerStarter
	Ready   ReadyWaiter
	Execute CommandRunner
	TempDir func() (string, error)
	Remove  func(path string) error
}

// Run executes opts.Command and returns its exit code. A non-nil error means
// the run could not complete and the exit code is ExitRunnerFailed.
func (r Runner) Run(ctx context.Context, opts Options) (code int, err error) {
	if len(opts.Command) == 0 {
		return ExitRunnerFailed, errors.New("no command given; usage: stratus ci [flags] -- <command> [args...]")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	dataDir, err := r.TempDir()
	if err != nil {
		return ExitRunnerFailed, fmt.Errorf("create data dir: %w", err)
	}
	defer func() {
		if opts.KeepData {
			fmt.Fprintf(stderr, "stratus ci: kept data dir %s\n", dataDir)
			return
		}
		if rmErr := r.Remove(dataDir); rmErr != nil && err == nil {
			code, err = ExitRunnerFailed, fmt.Errorf("remove data dir: %w", rmErr)
		}
	}()

	server, err := r.Start(ctx, ServerConfig{DataDir: dataDir, Seed: opts.Seed, LogLevel: opts.LogLevel})
	if err != nil {
		return ExitRunnerFailed, fmt.Errorf("start stratus: %w", err)
	}
	defer func() {
		if stopErr := server.Stop(); stopErr != nil && err == nil {
			code, err = ExitRunnerFailed, fmt.Errorf("stop stratus: %w", stopErr)
		}
	}()

	readyCtx, cancel := context.WithTimeout(ctx, opts.readyTimeout())
	defer cancel()
	if err := r.Ready(readyCtx, server.Endpoint()); err != nil {
		return ExitRunnerFailed, err
	}

	base := opts.Env
	if base == nil {
		base = os.Environ()
	}
	exitCode, err := r.Execute(ctx, opts.Command, CommandEnv(base, server.Endpoint(), server.Port()))
	if err != nil {
		return ExitRunnerFailed, fmt.Errorf("run %s: %w", opts.Command[0], err)
	}
	return exitCode, nil
}

func (o Options) readyTimeout() time.Duration {
	if o.ReadyTimeout > 0 {
		return o.ReadyTimeout
	}
	return 60 * time.Second
}

// CommandEnv returns the environment for the command under test. Endpoint
// variables always point at the private instance. Credentials and region
// get harmless defaults only when the caller has not set them, so real
// settings are never overridden.
func CommandEnv(base []string, endpoint string, port int) []string {
	forced := map[string]string{
		"AWS_ENDPOINT_URL":        endpoint,
		"STRATUS_ENDPOINT":        endpoint,
		"STRATUS_DOCKER_ENDPOINT": fmt.Sprintf("http://host.docker.internal:%d", port),
		"STRATUS_PORT":            fmt.Sprintf("%d", port),
	}
	defaults := map[string]string{
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_REGION":            "us-east-1",
		"AWS_DEFAULT_REGION":    "us-east-1",
	}

	out := make([]string, 0, len(base)+len(forced)+len(defaults))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, override := forced[key]; override {
			continue
		}
		delete(defaults, key)
		out = append(out, kv)
	}
	for _, m := range []map[string]string{forced, defaults} {
		for _, key := range sortedKeys(m) {
			out = append(out, key+"="+m[key])
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
