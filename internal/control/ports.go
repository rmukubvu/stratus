package control

import (
	"context"
	"time"
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

// SnapshotPaths is where a snapshot's data lives on disk.
type SnapshotPaths struct {
	MetaDB  string
	BlobDir string
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
