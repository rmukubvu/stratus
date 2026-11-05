package bench

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/stratus/internal/ci"
	"github.com/stratus/internal/control/client"
)

// Strategy is one way of getting a test back to the seeded fixture.
type Strategy interface {
	Name() string
	// Description says what a suite using this strategy does between tests.
	Description() string
	// Setup brings up a seeded Stratus and returns its endpoint.
	Setup(ctx context.Context) (endpoint string, err error)
	// Isolate restores the seeded state after a test. The endpoint may change.
	Isolate(ctx context.Context) (endpoint string, err error)
	// Close stops servers and removes every directory the strategy created.
	Close() error
}

// Host is what strategies need from the outside world; tests inject fakes.
type Host struct {
	Start   ci.ServerStarter
	Ready   ci.ReadyWaiter
	TempDir func() (string, error)
	Remove  func(path string) error
	Fixture Fixture
}

// instance is one running Stratus and the data directory it owns.
type instance struct {
	server ci.Server
	dir    string
}

func (h Host) launch(ctx context.Context, seed string) (*instance, error) {
	dir, err := h.TempDir()
	if err != nil {
		return nil, err
	}
	server, err := h.Start(ctx, ci.ServerConfig{DataDir: dir, Seed: seed, LogLevel: "error"})
	if err != nil {
		_ = h.Remove(dir)
		return nil, err
	}
	inst := &instance{server: server, dir: dir}
	if err := h.Ready(ctx, server.Endpoint()); err != nil {
		_ = h.stop(inst)
		return nil, err
	}
	return inst, nil
}

func (h Host) stop(inst *instance) error {
	if inst == nil {
		return nil
	}
	return errors.Join(inst.server.Stop(), h.Remove(inst.dir))
}

func (h Host) seed(ctx context.Context, endpoint string) error {
	return h.Fixture.Seed(ctx, AWS{Endpoint: endpoint})
}

// Strategies returns every strategy, slowest first.
func Strategies(h Host) []Strategy {
	return []Strategy{
		&freshAndSeed{host: h},
		&resetAndSeed{host: h},
		&freshFromSnapshot{host: h},
		&restoreSnapshot{host: h},
	}
}

// freshAndSeed starts a new emulator and runs setup again for every test:
// the usual pattern with an emulator that has no control API.
type freshAndSeed struct {
	host Host
	cur  *instance
}

func (s *freshAndSeed) Name() string { return "fresh emulator + setup" }
func (s *freshAndSeed) Description() string {
	return "stop the emulator, start a new one, run the setup calls again"
}

func (s *freshAndSeed) Setup(ctx context.Context) (string, error) { return s.Isolate(ctx) }

func (s *freshAndSeed) Isolate(ctx context.Context) (string, error) {
	if err := s.host.stop(s.cur); err != nil {
		return "", err
	}
	s.cur = nil
	inst, err := s.host.launch(ctx, "")
	if err != nil {
		return "", err
	}
	s.cur = inst
	return inst.server.Endpoint(), s.host.seed(ctx, inst.server.Endpoint())
}

func (s *freshAndSeed) Close() error { return s.host.stop(s.cur) }

// resetAndSeed keeps one emulator, wipes it with the control API and runs
// setup again.
type resetAndSeed struct {
	host     Host
	cur      *instance
	control  *client.Client
	endpoint string
}

func (s *resetAndSeed) Name() string        { return "reset + setup" }
func (s *resetAndSeed) Description() string { return "POST /reset, then run the setup calls again" }

func (s *resetAndSeed) Setup(ctx context.Context) (string, error) {
	inst, err := s.host.launch(ctx, "")
	if err != nil {
		return "", err
	}
	s.cur, s.endpoint = inst, inst.server.Endpoint()
	if s.control, err = client.New(s.endpoint, nil); err != nil {
		return "", err
	}
	return s.endpoint, s.host.seed(ctx, s.endpoint)
}

func (s *resetAndSeed) Isolate(ctx context.Context) (string, error) {
	if _, err := s.control.Reset(ctx); err != nil {
		return "", err
	}
	return s.endpoint, s.host.seed(ctx, s.endpoint)
}

func (s *resetAndSeed) Close() error { return s.host.stop(s.cur) }

// freshFromSnapshot seeds once, then starts every test on a new emulator
// whose data directory is cloned from the seeded snapshot (stratus ci --seed).
// This is what gives each parallel CI job its own copy of the fixture.
type freshFromSnapshot struct {
	host   Host
	golden *instance
	seed   string
	cur    *instance
}

func (s *freshFromSnapshot) Name() string { return "fresh emulator from snapshot (--seed)" }
func (s *freshFromSnapshot) Description() string {
	return "stop the emulator, start a new one seeded from the snapshot; no setup calls"
}

func (s *freshFromSnapshot) Setup(ctx context.Context) (string, error) {
	golden, err := s.host.launch(ctx, "")
	if err != nil {
		return "", err
	}
	s.golden = golden
	if err := s.host.seed(ctx, golden.server.Endpoint()); err != nil {
		return "", err
	}
	if err := snapshot(ctx, golden.server.Endpoint(), "seeded"); err != nil {
		return "", err
	}
	// The golden server can stop; its snapshot directory stays until Close.
	if err := golden.server.Stop(); err != nil {
		return "", err
	}
	s.seed = filepath.Join(golden.dir, "snapshots", "seeded")
	return s.Isolate(ctx)
}

func (s *freshFromSnapshot) Isolate(ctx context.Context) (string, error) {
	if err := s.host.stop(s.cur); err != nil {
		return "", err
	}
	s.cur = nil
	inst, err := s.host.launch(ctx, s.seed)
	if err != nil {
		return "", err
	}
	s.cur = inst
	return inst.server.Endpoint(), nil
}

func (s *freshFromSnapshot) Close() error {
	var golden error
	if s.golden != nil {
		golden = s.host.Remove(s.golden.dir)
	}
	return errors.Join(s.host.stop(s.cur), golden)
}

// restoreSnapshot keeps one emulator and restores the seeded snapshot.
type restoreSnapshot struct {
	host     Host
	cur      *instance
	control  *client.Client
	endpoint string
}

func (s *restoreSnapshot) Name() string        { return "snapshot restore" }
func (s *restoreSnapshot) Description() string { return "POST /snapshots/seeded/restore" }

func (s *restoreSnapshot) Setup(ctx context.Context) (string, error) {
	inst, err := s.host.launch(ctx, "")
	if err != nil {
		return "", err
	}
	s.cur, s.endpoint = inst, inst.server.Endpoint()
	if s.control, err = client.New(s.endpoint, nil); err != nil {
		return "", err
	}
	if err := s.host.seed(ctx, s.endpoint); err != nil {
		return "", err
	}
	_, err = s.control.CreateSnapshot(ctx, "seeded")
	return s.endpoint, err
}

func (s *restoreSnapshot) Isolate(ctx context.Context) (string, error) {
	_, err := s.control.RestoreSnapshot(ctx, "seeded")
	return s.endpoint, err
}

func (s *restoreSnapshot) Close() error { return s.host.stop(s.cur) }

func snapshot(ctx context.Context, endpoint, name string) error {
	c, err := client.New(endpoint, nil)
	if err != nil {
		return err
	}
	if _, err := c.CreateSnapshot(ctx, name); err != nil {
		return fmt.Errorf("snapshot %s: %w", name, err)
	}
	return nil
}
