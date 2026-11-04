package control

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type fakeState struct {
	buckets   map[string]bool
	listErr   error
	deleted   []string
	written   []string
	restored  []string
	writeErr  error
	restoreFn func(path string) error
}

func newFakeState(buckets ...string) *fakeState {
	s := &fakeState{buckets: map[string]bool{}}
	for _, b := range buckets {
		s.buckets[b] = true
	}
	return s
}

func (f *fakeState) BucketNames() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for b := range f.buckets {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeState) DeleteBuckets(names ...string) error {
	for _, n := range names {
		delete(f.buckets, n)
		f.deleted = append(f.deleted, n)
	}
	return nil
}

func (f *fakeState) WriteSnapshot(path string) error {
	f.written = append(f.written, path)
	return f.writeErr
}

func (f *fakeState) RestoreSnapshot(path string) error {
	f.restored = append(f.restored, path)
	if f.restoreFn != nil {
		return f.restoreFn(path)
	}
	return nil
}

type fakeBlobs struct {
	clearCalls int
	deletedNS  []string
	cloned     []string
	replaced   []string
	cloneErr   error
}

func (f *fakeBlobs) DeleteNamespace(ns string) error {
	f.deletedNS = append(f.deletedNS, ns)
	return nil
}
func (f *fakeBlobs) Clear() error                 { f.clearCalls++; return nil }
func (f *fakeBlobs) CloneTo(dir string) error     { f.cloned = append(f.cloned, dir); return f.cloneErr }
func (f *fakeBlobs) ReplaceWith(dir string) error { f.replaced = append(f.replaced, dir); return nil }

type fakeRuntime struct {
	resets  int
	pingErr error
}

func (f *fakeRuntime) Reset(context.Context) error { f.resets++; return nil }
func (f *fakeRuntime) Ping(context.Context) error  { return f.pingErr }

type fakeRepo struct {
	mu        sync.Mutex
	snaps     map[SnapshotID]Snapshot
	aborted   []SnapshotID
	commitErr error
}

func newFakeRepo() *fakeRepo { return &fakeRepo{snaps: map[SnapshotID]Snapshot{}} }

func (r *fakeRepo) Stage(id SnapshotID) (SnapshotStaging, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.snaps[id]; ok {
		return nil, snapshotExists(id)
	}
	return &fakeStaging{repo: r, id: id}, nil
}

func (r *fakeRepo) Get(id SnapshotID) (Snapshot, SnapshotPaths, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snaps[id]
	if !ok {
		return Snapshot{}, SnapshotPaths{}, snapshotNotFound(id)
	}
	return s, fakePaths(id), nil
}

func (r *fakeRepo) List() ([]Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Snapshot
	for _, s := range r.snaps {
		out = append(out, s)
	}
	return out, nil
}

func (r *fakeRepo) Delete(id SnapshotID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.snaps[id]; !ok {
		return snapshotNotFound(id)
	}
	delete(r.snaps, id)
	return nil
}

func fakePaths(id SnapshotID) SnapshotPaths {
	return SnapshotPaths{MetaDB: "/snap/" + string(id) + "/meta.db", BlobDir: "/snap/" + string(id) + "/blobs"}
}

type fakeStaging struct {
	repo *fakeRepo
	id   SnapshotID
}

func (s *fakeStaging) Paths() SnapshotPaths { return fakePaths(s.id) }
func (s *fakeStaging) Commit(snap Snapshot) error {
	if s.repo.commitErr != nil {
		return s.repo.commitErr
	}
	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	s.repo.snaps[s.id] = snap
	return nil
}
func (s *fakeStaging) Abort() error {
	s.repo.aborted = append(s.repo.aborted, s.id)
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var errBoom = errors.New("boom")
