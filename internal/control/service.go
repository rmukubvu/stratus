package control

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"time"
)

// Status is the overall health of the emulator.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// Check is the result of one health probe.
type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Critical bool   `json:"critical"`
	Detail   string `json:"detail,omitempty"`
}

// Health is a point-in-time view of the emulator.
type Health struct {
	Status        Status    `json:"status"`
	Ready         bool      `json:"ready"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Services      []string  `json:"services"`
	Checks        []Check   `json:"checks"`
}

// ResetResult reports what a reset changed.
type ResetResult struct {
	// Services that were reset. Empty input resets every service.
	Services []string `json:"services"`
	// BucketsCleared lists metadata buckets that were dropped.
	BucketsCleared []string `json:"buckets_cleared"`
	// RuntimeRecycled is true when warm Lambda containers were stopped.
	RuntimeRecycled bool `json:"runtime_recycled"`
}

// Options wires the Service to its adapters. Runtime and Clock are optional.
type Options struct {
	Catalog   Catalog
	State     StateStore
	Blobs     BlobStore
	Runtime   Runtime
	Snapshots SnapshotRepository
	Gate      *Gate
	Clock     Clock
	StartedAt time.Time
}

// Service is the control plane's application service.
type Service struct {
	catalog   Catalog
	state     StateStore
	blobs     BlobStore
	runtime   Runtime
	snapshots SnapshotRepository
	gate      *Gate
	clock     Clock
	startedAt time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewService builds a control Service.
func NewService(opts Options) *Service {
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	gate := opts.Gate
	if gate == nil {
		gate = &Gate{}
	}
	startedAt := opts.StartedAt
	if startedAt.IsZero() {
		startedAt = clock.Now()
	}
	return &Service{
		catalog:   opts.Catalog,
		state:     opts.State,
		blobs:     opts.Blobs,
		runtime:   opts.Runtime,
		snapshots: opts.Snapshots,
		gate:      gate,
		clock:     clock,
		startedAt: startedAt,
	}
}

// Gate returns the gate API traffic must pass through.
func (s *Service) Gate() *Gate { return s.gate }

// Health probes the emulator's dependencies. The state store is critical:
// without it nothing works. The Lambda runtime is not: everything except
// function execution keeps working without Docker.
func (s *Service) Health(ctx context.Context) Health {
	checks := []Check{s.checkState()}
	if s.runtime != nil {
		checks = append(checks, s.checkRuntime(ctx))
	}

	status, ready := StatusOK, true
	for _, c := range checks {
		if c.Status == StatusOK {
			continue
		}
		if c.Critical {
			status, ready = StatusDown, false
			break
		}
		status = StatusDegraded
	}

	now := s.clock.Now()
	return Health{
		Status:        status,
		Ready:         ready,
		StartedAt:     s.startedAt.UTC(),
		UptimeSeconds: int64(now.Sub(s.startedAt).Seconds()),
		Services:      s.catalog.Names(),
		Checks:        checks,
	}
}

func (s *Service) checkState() Check {
	c := Check{Name: "state_store", Critical: true, Status: StatusOK}
	if _, err := s.state.BucketNames(); err != nil {
		c.Status, c.Detail = StatusDown, err.Error()
	}
	return c
}

func (s *Service) checkRuntime(ctx context.Context) Check {
	c := Check{Name: "lambda_runtime", Critical: false, Status: StatusOK}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.runtime.Ping(ctx); err != nil {
		c.Status = StatusDegraded
		c.Detail = err.Error() + "; Lambda invocations will fail until Docker is available"
	}
	return c
}

// Reset returns services to an empty state. With no services named it
// resets everything.
func (s *Service) Reset(ctx context.Context, services []string) (ResetResult, error) {
	targets, err := s.catalog.Resolve(services)
	if err != nil {
		return ResetResult{}, err
	}
	resetAll := len(targets) == 0

	var result ResetResult
	err = s.gate.Exclusive(ctx, func() error {
		buckets, err := s.bucketsToClear(targets, resetAll)
		if err != nil {
			return err
		}
		if err := s.state.DeleteBuckets(buckets...); err != nil {
			return internalError("clear metadata", err)
		}
		if err := s.clearBlobs(targets, resetAll); err != nil {
			return err
		}
		recycled, err := s.recycleRuntime(ctx, targets, resetAll)
		if err != nil {
			return err
		}
		if resetAll {
			targets = s.catalog.Names()
		}
		result = ResetResult{Services: targets, BucketsCleared: buckets, RuntimeRecycled: recycled}
		return nil
	})
	return result, err
}

func (s *Service) bucketsToClear(targets []string, resetAll bool) ([]string, error) {
	all, err := s.state.BucketNames()
	if err != nil {
		return nil, internalError("list metadata buckets", err)
	}
	if resetAll {
		return all, nil
	}
	wanted := toSet(targets)
	var out []string
	for _, bucket := range all {
		if owner, ok := s.catalog.OwnerOf(bucket); ok {
			if _, hit := wanted[owner]; hit {
				out = append(out, bucket)
			}
		}
	}
	return out, nil
}

func (s *Service) clearBlobs(targets []string, resetAll bool) error {
	if resetAll {
		if err := s.blobs.Clear(); err != nil {
			return internalError("clear blobs", err)
		}
		return nil
	}
	for _, name := range targets {
		st, _ := s.catalog.Lookup(name)
		for _, ns := range st.BlobNamespaces {
			if err := s.blobs.DeleteNamespace(ns); err != nil {
				return internalError("clear blobs for "+name, err)
			}
		}
	}
	return nil
}

func (s *Service) recycleRuntime(ctx context.Context, targets []string, resetAll bool) (bool, error) {
	if s.runtime == nil {
		return false, nil
	}
	needed := resetAll
	for _, name := range targets {
		if st, _ := s.catalog.Lookup(name); st.UsesRuntime {
			needed = true
		}
	}
	if !needed {
		return false, nil
	}
	if err := s.runtime.Reset(ctx); err != nil {
		return false, internalError("recycle Lambda containers", err)
	}
	return true, nil
}

// CreateSnapshot captures all current state. An empty name generates one.
func (s *Service) CreateSnapshot(ctx context.Context, name string) (Snapshot, error) {
	id := NewSnapshotID(s.clock.Now())
	if name != "" {
		parsed, err := ParseSnapshotID(name)
		if err != nil {
			return Snapshot{}, err
		}
		id = parsed
	}

	staging, err := s.snapshots.Stage(id)
	if err != nil {
		return Snapshot{}, err
	}

	var snap Snapshot
	err = s.gate.Exclusive(ctx, func() error {
		paths := staging.Paths()
		if err := s.state.WriteSnapshot(paths.MetaDB); err != nil {
			return internalError("snapshot metadata", err)
		}
		if err := s.blobs.CloneTo(paths.BlobDir); err != nil {
			return internalError("snapshot blobs", err)
		}
		services, err := s.servicesWithState()
		if err != nil {
			return err
		}
		snap = Snapshot{
			ID:        id,
			CreatedAt: s.clock.Now().UTC(),
			Services:  services,
			SizeBytes: diskSize(paths.MetaDB) + diskSize(paths.BlobDir),
		}
		return nil
	})
	if err != nil {
		_ = staging.Abort()
		return Snapshot{}, err
	}
	if err := staging.Commit(snap); err != nil {
		_ = staging.Abort()
		var domainErr *Error
		if errors.As(err, &domainErr) {
			return Snapshot{}, domainErr
		}
		return Snapshot{}, internalError("commit snapshot", err)
	}
	return snap, nil
}

// RestoreSnapshot replaces all current state with the snapshot's state.
func (s *Service) RestoreSnapshot(ctx context.Context, rawID string) (Snapshot, error) {
	id, err := ParseSnapshotID(rawID)
	if err != nil {
		return Snapshot{}, err
	}
	snap, paths, err := s.snapshots.Get(id)
	if err != nil {
		return Snapshot{}, err
	}
	err = s.gate.Exclusive(ctx, func() error {
		if err := s.state.RestoreSnapshot(paths.MetaDB); err != nil {
			return internalError("restore metadata", err)
		}
		if err := s.blobs.ReplaceWith(paths.BlobDir); err != nil {
			return internalError("restore blobs", err)
		}
		if s.runtime != nil {
			if err := s.runtime.Reset(ctx); err != nil {
				return internalError("recycle Lambda containers", err)
			}
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// ListSnapshots returns all snapshots, oldest first.
func (s *Service) ListSnapshots() ([]Snapshot, error) {
	snaps, err := s.snapshots.List()
	if err != nil {
		return nil, internalError("list snapshots", err)
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].CreatedAt.Equal(snaps[j].CreatedAt) {
			return snaps[i].ID < snaps[j].ID
		}
		return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
	})
	return snaps, nil
}

// GetSnapshot returns one snapshot.
func (s *Service) GetSnapshot(rawID string) (Snapshot, error) {
	id, err := ParseSnapshotID(rawID)
	if err != nil {
		return Snapshot{}, err
	}
	snap, _, err := s.snapshots.Get(id)
	return snap, err
}

// DeleteSnapshot removes a snapshot. Live state is not affected.
func (s *Service) DeleteSnapshot(rawID string) error {
	id, err := ParseSnapshotID(rawID)
	if err != nil {
		return err
	}
	return s.snapshots.Delete(id)
}

func (s *Service) servicesWithState() ([]string, error) {
	buckets, err := s.state.BucketNames()
	if err != nil {
		return nil, internalError("list metadata buckets", err)
	}
	owners := map[string]struct{}{}
	for _, b := range buckets {
		if owner, ok := s.catalog.OwnerOf(b); ok {
			owners[owner] = struct{}{}
		}
	}
	out := make([]string, 0, len(owners))
	for name := range owners {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func toSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

func diskSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
