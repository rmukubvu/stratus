// Package kafka emulates Amazon MSK. Every cluster, provisioned or
// serverless, is a real Kafka-compatible broker (Redpanda) running in a
// container, so producers and consumers connect to the bootstrap address the
// API returns and exchange real records.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
)

const clustersBucket = "kafka-clusters"

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	// Engines runs the brokers. Nil means no container engine is available,
	// so creating a cluster fails with a clear error.
	Engines backing.Runner
	// Engine is the broker to run. Zero means DefaultEngine without overrides.
	Engine Engine
	Now    func() time.Time
	// FreePort reserves the loopback port a new broker advertises. Nil means
	// backing.FreeLoopbackPort.
	FreePort func() (int, error)
}

// Service is the MSK application service. It also serves as the control
// plane's backend for kafka brokers.
type Service struct {
	scope    scope.Scope
	metadata store.Store
	engines  backing.Runner
	engine   Engine
	now      func() time.Time
	freePort func() (int, error)
	mu       sync.Mutex
}

// NewService builds an MSK service.
func NewService(opts Options) *Service {
	s := &Service{
		scope:    scope.Default(),
		metadata: opts.Metadata,
		engines:  opts.Engines,
		engine:   opts.Engine,
		now:      opts.Now,
		freePort: opts.FreePort,
	}
	if s.engines == nil {
		s.engines = backing.Unavailable{}
	}
	if s.engine.Image == "" {
		s.engine = DefaultEngine(nil)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.freePort == nil {
		s.freePort = backing.FreeLoopbackPort
	}
	return s
}

// CreateProvisioned starts a broker for a provisioned cluster and returns once
// it accepts clients.
func (s *Service) CreateProvisioned(ctx context.Context, name string, cfg provisionedConfig, tags map[string]string) (cluster, error) {
	if err := cfg.validate(); err != nil {
		return cluster{}, err
	}
	c, err := newCluster(s.scope, name, tags, s.now())
	if err != nil {
		return cluster{}, err
	}
	c.Type, c.Provisioned = typeProvisioned, &cfg
	return s.create(ctx, c)
}

// CreateServerless starts a broker for a serverless cluster. Its VPC and
// authentication settings are kept verbatim.
func (s *Service) CreateServerless(ctx context.Context, name string, serverless json.RawMessage, tags map[string]string) (cluster, error) {
	c, err := newCluster(s.scope, name, tags, s.now())
	if err != nil {
		return cluster{}, err
	}
	c.Type, c.Serverless = typeServerless, serverless
	return s.create(ctx, c)
}

func (s *Service) create(ctx context.Context, c cluster) (cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, _ := s.metadata.Get(clustersBucket, c.Name); existing != nil {
		return cluster{}, conflict("cluster %s already exists", c.Name)
	}
	if err := s.startBroker(ctx, &c, 0, ""); err != nil {
		return cluster{}, backing.APIError(err, "cluster "+c.Name)
	}
	if err := s.put(c); err != nil {
		_ = s.engines.Stop(context.Background(), kind, c.Name)
		return cluster{}, err
	}
	return c, nil
}

// Get finds a cluster by ARN.
func (s *Service) Get(arn string) (cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(arn)
}

// List returns clusters sorted by name, optionally only those whose name
// starts with namePrefix, and only of clusterType when it is set.
func (s *Service) List(namePrefix, clusterType string) ([]cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.list()
	if err != nil {
		return nil, err
	}
	out := make([]cluster, 0, len(all))
	for _, c := range all {
		if strings.HasPrefix(c.Name, namePrefix) && (clusterType == "" || c.Type == clusterType) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Delete stops the cluster's broker and forgets the cluster. The returned
// cluster reports the DELETING state, as MSK does.
func (s *Service) Delete(ctx context.Context, arn string) (cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(arn)
	if err != nil {
		return cluster{}, err
	}
	if err := s.engines.Stop(ctx, kind, c.Name); err != nil && !errors.Is(err, backing.ErrUnavailable) {
		return cluster{}, backing.APIError(err, "cluster "+c.Name)
	}
	if err := s.metadata.Delete(clustersBucket, c.Name); err != nil {
		return cluster{}, internal(err)
	}
	c.State = stateDeleting
	return c, nil
}

// Tag adds or replaces tags on a cluster.
func (s *Service) Tag(arn string, tags map[string]string) error {
	return s.update(arn, func(c *cluster) {
		for k, v := range tags {
			c.Tags[k] = v
		}
	})
}

// Untag removes tags from a cluster.
func (s *Service) Untag(arn string, keys []string) error {
	return s.update(arn, func(c *cluster) {
		for _, k := range keys {
			delete(c.Tags, k)
		}
	})
}

func (s *Service) update(arn string, change func(*cluster)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(arn)
	if err != nil {
		return err
	}
	if c.Tags == nil {
		c.Tags = map[string]string{}
	}
	change(&c)
	return s.put(c)
}

// Service names this backend for the control plane.
func (s *Service) Service() string { return kind }

// Reset stops every broker. The control plane calls it after dropping the
// service's metadata. Without a container engine there is nothing reachable
// to stop, which is not an error.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.engines.StopAll(ctx, kind); err != nil && !errors.Is(err, backing.ErrUnavailable) {
		return err
	}
	return nil
}

// Reconcile starts a broker for every recorded cluster that has none: after
// a snapshot restore, and at startup. Each broker keeps its recorded
// bootstrap address unless opts asks for fresh ones, and starts from its
// archive in opts.DataDir when there is one, with its topics and offsets. A
// cluster whose broker cannot start is marked failed; the error names them
// all.
func (s *Service) Reconcile(ctx context.Context, opts backing.ReconcileOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	clusters, err := s.list()
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range clusters {
		running, err := s.engines.Running(ctx, kind, c.Name)
		if err == nil && running {
			continue
		}
		if err == nil {
			hostPort := c.Port
			if opts.FreshEndpoints {
				hostPort = 0
			}
			err = s.startBroker(ctx, &c, hostPort, backing.SeedFrom(opts.DataDir, c.Name))
		}
		if err != nil {
			c.State = stateFailed
			errs = append(errs, fmt.Errorf("kafka: start broker for %s: %w", c.Name, err))
		}
		if err := s.put(c); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// Capture exports every running broker's topics and offsets into dir and
// returns the names of the clusters it captured.
func (s *Service) Capture(ctx context.Context, dir string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clusters, err := s.list()
	if err != nil {
		return nil, err
	}
	engines := make([]backing.Exportable, 0, len(clusters))
	for _, c := range clusters {
		engines = append(engines, backing.Exportable{Name: c.Name, Data: s.engine.Data()})
	}
	return backing.ExportAll(ctx, s.engines, kind, dir, engines)
}

// startBroker runs the cluster's broker on hostPort, reserving a free port
// when it is zero, from the data archive seed when it is set, and records
// where clients reach it.
func (s *Service) startBroker(ctx context.Context, c *cluster, hostPort int, seed string) error {
	if hostPort == 0 {
		port, err := s.freePort()
		if err != nil {
			return err
		}
		hostPort = port
	}
	endpoint, err := s.engines.Start(ctx, s.engine.Spec(c.Name, hostPort, seed))
	if err != nil {
		return err
	}
	c.Address, c.Port, c.State = endpoint.Address, endpoint.Port, stateActive
	return nil
}

func (s *Service) load(arn string) (cluster, error) {
	name, ok := nameFromARN(arn)
	if !ok {
		return cluster{}, badRequest("%q is not an MSK cluster ARN", arn)
	}
	raw, err := s.metadata.Get(clustersBucket, name)
	if err != nil {
		return cluster{}, internal(err)
	}
	var c cluster
	if raw == nil || json.Unmarshal(raw, &c) != nil || c.ARN != arn {
		return cluster{}, notFound("cluster %s does not exist", arn)
	}
	return c, nil
}

func (s *Service) list() ([]cluster, error) {
	out := make([]cluster, 0)
	err := s.metadata.Scan(clustersBucket, "", func(_, v []byte) error {
		var c cluster
		if json.Unmarshal(v, &c) == nil {
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Service) put(c cluster) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(clustersBucket, c.Name, raw); err != nil {
		return internal(err)
	}
	return nil
}

// SetScope sets the account and region this service instance serves.
func (s *Service) SetScope(sc scope.Scope) {
	s.scope = sc
}
