// Package backing defines how emulated services run real engines, such as the
// PostgreSQL server behind an RDS instance or the Valkey server behind an
// ElastiCache cluster. Services depend only on the Runner port declared here;
// the Docker adapter lives in internal/container.
package backing

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	// Data says where the engine keeps its state. Nil means the engine holds
	// nothing worth keeping, like Memcached, so it is never exported.
	Data *Data
	// Seed is an archive written by Runner.Export. When set, the engine starts
	// from that data instead of an empty data directory. It requires Data.
	Seed string
}

// Data describes the state an engine keeps on disk.
type Data struct {
	// Path is the directory inside the container that holds the state, for
	// example PostgreSQL's PGDATA.
	Path string
	// Flush makes the engine write in-memory state to Path before an export,
	// for engines such as Valkey that keep their data in memory. Nil means
	// everything is already on disk.
	Flush func(ctx context.Context, address string) error
	// Exclude names entries directly under Path that must not be exported,
	// such as a lock file that only means something to the running process.
	Exclude []string
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
	// Export writes a running engine's data to the archive file dst, as a
	// consistent point-in-time copy. An engine that is not running fails
	// with ErrNotRunning.
	Export(ctx context.Context, kind, name string, data Data, dst string) error
}

// ReconcileOptions say how a backend brings engines back for resources that
// exist in state but have none running.
type ReconcileOptions struct {
	// DataDir holds the archives a backend captured into a snapshot. An
	// engine with an archive there starts from it; one without starts empty.
	// Empty means every engine starts empty.
	DataDir string
	// FreshEndpoints publishes every engine on a new port instead of the one
	// recorded in state. A data directory seeded from a snapshot needs it,
	// because sibling seeds of the same snapshot run side by side.
	FreshEndpoints bool
}

// ErrNotRunning means the engine has no running container to export from.
var ErrNotRunning = errors.New("engine is not running")

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
func (Unavailable) Export(context.Context, string, string, Data, string) error {
	return ErrUnavailable
}

// Exportable is one engine whose data a backend wants in a snapshot.
type Exportable struct {
	Name string
	Data Data
}

// ExportAll exports each engine of kind into dir, one archive per engine, and
// returns the names it captured. An engine that is not running, or a runner
// without a container engine, holds no data to capture, so it is skipped and
// comes back empty on restore.
func ExportAll(ctx context.Context, runner Runner, kind, dir string, engines []Exportable) ([]string, error) {
	var captured []string
	for _, e := range engines {
		err := runner.Export(ctx, kind, e.Name, e.Data, ArchivePath(dir, e.Name))
		switch {
		case err == nil:
			captured = append(captured, e.Name)
		case errors.Is(err, ErrNotRunning), errors.Is(err, ErrUnavailable):
			continue
		default:
			return nil, fmt.Errorf("export %s %s: %w", kind, e.Name, err)
		}
	}
	return captured, nil
}

// FreeLoopbackPort finds a loopback port nothing listens on. Engines that
// must know their published port before they start, such as a Kafka broker
// that advertises its own address to clients, reserve one with it.
func FreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
