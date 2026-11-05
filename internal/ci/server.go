package ci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/stratus/internal/app"
	"github.com/stratus/internal/config"
	"github.com/stratus/internal/control/client"
)

type inProcessServer struct {
	port   int
	cancel context.CancelFunc
	done   chan error
}

func (s *inProcessServer) Endpoint() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }
func (s *inProcessServer) Port() int        { return s.port }

func (s *inProcessServer) Stop() error {
	s.cancel()
	select {
	case err := <-s.done:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(30 * time.Second):
		return errors.New("timed out waiting for stratus to shut down")
	}
}

// StartInProcess starts Stratus inside the current process on a free port.
// It listens on all interfaces so Lambda containers can reach it through
// host.docker.internal.
func StartInProcess(ctx context.Context, cfg ServerConfig) (Server, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("pick a free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	appCfg := config.Config{
		Port:      port,
		DataDir:   cfg.DataDir,
		LogLevel:  cfg.LogLevel,
		LogFormat: "auto",
		Seed:      cfg.Seed,
	}
	if appCfg.LogLevel == "" {
		appCfg.LogLevel = "warn"
	}
	if err := appCfg.Validate(); err != nil {
		ln.Close()
		return nil, err
	}
	application, err := app.New(appCfg, app.NewLogger(appCfg.LogLevel, appCfg.LogFormat))
	if err != nil {
		ln.Close()
		return nil, err
	}

	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- application.Serve(serveCtx, ln) }()
	return &inProcessServer{port: port, cancel: cancel, done: done}, nil
}

// WaitReady polls the control API until Stratus reports ready.
func WaitReady(ctx context.Context, endpoint string) error {
	c, err := client.New(endpoint, nil)
	if err != nil {
		return err
	}
	return c.WaitReady(ctx, 100*time.Millisecond)
}

// TempDataDir creates a fresh data directory for one run.
func TempDataDir() (string, error) {
	return os.MkdirTemp("", "stratus-ci-*")
}

// ExecCommand runs command with env, wired to this process's stdio, and
// returns its exit code. When ctx is cancelled the command is interrupted
// and given a grace period before it is killed.
func ExecCommand(ctx context.Context, command []string, env []string) (int, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Cancel = func() error { return interrupt(cmd.Process) }
	cmd.WaitDelay = 10 * time.Second

	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode(), nil
	case errors.As(err, &exitErr):
		// Killed by a signal: report it like a shell would.
		return 128 + 15, nil
	default:
		return ExitRunnerFailed, err
	}
}

// DefaultRunner wires the Runner to real servers and processes.
func DefaultRunner() Runner {
	return Runner{
		Start:   StartInProcess,
		Ready:   WaitReady,
		Execute: ExecCommand,
		TempDir: TempDataDir,
		Remove:  os.RemoveAll,
	}
}
