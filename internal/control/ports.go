package control

import (
	"context"
	"path/filepath"
	"time"

	"github.com/stratus/internal/backing"
)

// StateStore is the metadata database every service writes to.
type StateStore interface {
	BucketNames() ([]string, error)
	DeleteBuckets(names ...string) error
	WriteSnapshot(path string) error
	RestoreSnapshot(path string) error
}

// BlobStore holds large payloads such as S3 objects and Lambda code.
type BlobStore interface {
	DeleteNamespace(namespace string) error
	Clear() error
	CloneTo(dir string) error
	ReplaceWith(dir string) error
}

// Runtime is the Lambda execution environment.
type Runtime interface {
	// Reset discards warm containers so they pick up changed state.
	Reset(ctx context.Context) error
	// Ping reports whether the runtime can start containers.
	Ping(ctx context.Context) error
}

// Backend runs real engines behind one service's resources, such as the
// database containers behind RDS instances. Engines live outside the state
// store, so the control plane tells backends when state changes under them.
type Backend interface {
	// Service is the catalog name of the service the backend serves.
	Service() string
	// Reset stops every engine. It runs after the service's state is dropped.
	Reset(ctx context.Context) error
	// Reconcile starts engines for resources that exist in state but have
	// none running. It runs after a snapshot restore and at startup.
	Reconcile(ctx context.Context, opts backing.ReconcileOptions) error
	// Capture writes the data held by each running engine into dir and
	// returns the names of the resources it captured. Reconcile reads the
	// same directory back through ReconcileOptions.DataDir.
	Capture(ctx context.Context, dir string) ([]string, error)
}

// SnapshotPaths is where a snapshot's data lives on disk.
type SnapshotPaths struct {
	MetaDB  string
	BlobDir string
	// EngineDir holds engine data, one subdirectory per backend service.
	EngineDir string
}

// EngineDirFor is where a backend's engine data lives inside EngineDir.
func (p SnapshotPaths) EngineDirFor(service string) string {
	return filepath.Join(p.EngineDir, service)
}

// SnapshotStaging is a snapshot being written. It becomes visible only once
// committed, so a crash mid-write never leaves a half-written snapshot.
type SnapshotStaging interface {
	Paths() SnapshotPaths
	Commit(Snapshot) error
	Abort() error
}

// SnapshotRepository persists snapshots.
type SnapshotRepository interface {
	Stage(id SnapshotID) (SnapshotStaging, error)
	Get(id SnapshotID) (Snapshot, SnapshotPaths, error)
	List() ([]Snapshot, error)
	Delete(id SnapshotID) error
}

// Clock lets tests control time.
type Clock interface {
	Now() time.Time
}
