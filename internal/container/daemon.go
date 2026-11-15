package container

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	dockerclient "github.com/docker/docker/client"
)

// Daemon is one connection to Docker shared by everything that starts
// containers, together with the reach policy resolved for it. Resolving once
// keeps Lambda runtimes and engines consistent with each other.
type Daemon struct {
	client     *dockerclient.Client
	owner      Owner
	resolution Resolution
	logger     *slog.Logger
}

// Connect opens the daemon for dataDir's Stratus instance and resolves how
// containers are reached from settings and the process's placement.
func Connect(dataDir string, settings ReachSettings, logger *slog.Logger) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}
	client, err := newDockerClient()
	if err != nil {
		return nil, err
	}
	owner := NewOwner(dataDir)
	r := resolver{api: client, placement: DetectPlacement(), instance: owner.Instance, localAddressFor: localAddressFor}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Resolve(ctx, settings)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("resolve how to reach containers: %w", err)
	}
	if res.Reach.Mode != ReachLoopback || res.Files != DeliverBind {
		logger.Info("container reach", "reach", res.Reach.String(), "files", string(res.Files), "callback_host", res.CallbackHost)
	}
	return &Daemon{client: client, owner: owner, resolution: res, logger: logger}, nil
}

// Reach is the resolved addressing policy.
func (d *Daemon) Reach() Reach { return d.resolution.Reach }

// Files is how Lambda containers get their files.
func (d *Daemon) Files() FileDelivery { return d.resolution.Files }

// CallbackHost is the host containers use to reach Stratus.
func (d *Daemon) CallbackHost() string { return d.resolution.CallbackHost }

// Close undoes resolution (removing a network Stratus created) and closes
// the client. Close it after everything that used it.
func (d *Daemon) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.resolution.Close(ctx)
	if cerr := d.client.Close(); err == nil {
		err = cerr
	}
	return err
}
