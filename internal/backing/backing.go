// Package backing defines how emulated services run real engines, such as the
// PostgreSQL server behind an RDS instance or the Valkey server behind an
// ElastiCache cluster. Services depend only on the Runner port declared here;
// the Docker adapter lives in internal/container.
package backing

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultReadyTimeout bounds how long Start waits for an engine to accept
// connections when a Spec does not say otherwise.
const DefaultReadyTimeout = 60 * time.Second

// Spec describes one engine to run.
type Spec struct {
	// Kind groups engines by the service that owns them, for example "rds".
	Kind string
	// Name identifies the engine within its kind, for example a DB instance
	// identifier. Kind and Name together are unique per Stratus instance.
	Name string
	// Image is the container image that provides the engine.
	Image string
	// Env and Cmd configure the engine process.
	Env []string
	Cmd []string
	// Port is the port the engine listens on inside its container.
	Port int
	// HostPort is the loopback port to publish Port on. Zero lets the runner
	// pick a free port; a fixed value lets an engine come back on the same
	// endpoint after a restart or restore.
	HostPort int
	// Probe decides when the engine is ready. Nil means a plain TCP connect.
	Probe Probe
	// ReadyTimeout bounds the wait for readiness. Zero means DefaultReadyTimeout.
	ReadyTimeout time.Duration
}

// Endpoint is where clients reach a running engine.
type Endpoint struct {
	Address string
	Port    int
}

// String renders the endpoint as host:port.
func (e Endpoint) String() string { return fmt.Sprintf("%s:%d", e.Address, e.Port) }

// Runner starts and stops engines. Implementations must be safe for
// concurrent use.
type Runner interface {
	// Start runs an engine and returns once it is ready for connections. An
	// engine with the same kind and name that is already running is replaced.
	Start(ctx context.Context, spec Spec) (Endpoint, error)
	// Stop removes an engine. Stopping an engine that does not exist is not
	// an error.
	Stop(ctx context.Context, kind, name string) error
	// Running reports whether the engine exists and is up.
	Running(ctx context.Context, kind, name string) (bool, error)
	// StopAll removes every engine of a kind.
	StopAll(ctx context.Context, kind string) error
}

// ErrUnavailable means no container engine can be reached, so no engine can
// be started or inspected.
var ErrUnavailable = errors.New("container engine is unavailable")

// ImageMissingError means the engine's image is not present locally. Stratus
// never pulls images itself, so that test runs stay deterministic and offline.
type ImageMissingError struct {
	Image string
}

func (e *ImageMissingError) Error() string {
	return fmt.Sprintf("image %s is not available locally; run `docker pull %s`", e.Image, e.Image)
}

// NotReadyError means the engine started but did not accept connections in
// time. Logs carries the tail of the engine's output to explain why.
type NotReadyError struct {
	Name    string
	Timeout time.Duration
	Logs    string
}

func (e *NotReadyError) Error() string {
	msg := fmt.Sprintf("engine %s did not become ready within %s", e.Name, e.Timeout)
	if e.Logs != "" {
		msg += "; last output: " + e.Logs
	}
	return msg
}

// Unavailable is a Runner for deployments without a container engine. Every
// call that needs one fails with ErrUnavailable, and there is nothing to stop.
type Unavailable struct{}

func (Unavailable) Start(context.Context, Spec) (Endpoint, error)         { return Endpoint{}, ErrUnavailable }
func (Unavailable) Stop(context.Context, string, string) error            { return nil }
func (Unavailable) Running(context.Context, string, string) (bool, error) { return false, nil }
func (Unavailable) StopAll(context.Context, string) error                 { return nil }
