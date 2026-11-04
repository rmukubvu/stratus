package control

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	state   *fakeState
	blobs   *fakeBlobs
	runtime *fakeRuntime
	repo    *fakeRepo
	svc     *Service
}

var t0 = time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)

func newFixture(buckets ...string) *fixture {
	f := &fixture{
		state:   newFakeState(buckets...),
		blobs:   &fakeBlobs{},
		runtime: &fakeRuntime{},
		repo:    newFakeRepo(),
	}
	f.svc = NewService(Options{
		Catalog:   DefaultCatalog(),
		State:     f.state,
		Blobs:     f.blobs,
		Runtime:   f.runtime,
		Snapshots: f.repo,
		Clock:     fixedClock{t0.Add(90 * time.Second)},
		StartedAt: t0,
	})
	return f
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("expected error code %s, got %v", code, err)
	}
}

func TestHealthIsOKWhenAllChecksPass(t *testing.T) {
	h := newFixture().svc.Health(context.Background())
	if h.Status != StatusOK || !h.Ready {
		t.Fatalf("got status %s ready %v", h.Status, h.Ready)
	}
	if h.UptimeSeconds != 90 {
		t.Fatalf("uptime: got %d, want 90", h.UptimeSeconds)
	}
	if len(h.Services) == 0 || len(h.Checks) != 2 {
		t.Fatalf("unexpected services/checks: %+v", h)
	}
}

func TestHealthIsDegradedButReadyWithoutDocker(t *testing.T) {
	f := newFixture()
	f.runtime.pingErr = errBoom
	h := f.svc.Health(context.Background())
	if h.Status != StatusDegraded || !h.Ready {
		t.Fatalf("got status %s ready %v", h.Status, h.Ready)
	}
	if !strings.Contains(h.Checks[1].Detail, "Lambda") {
		t.Fatalf("runtime check should explain impact: %q", h.Checks[1].Detail)
	}
}

func TestHealthIsDownAndNotReadyWhenStateStoreFails(t *testing.T) {
	f := newFixture()
	f.state.listErr = errBoom
	h := f.svc.Health(context.Background())
	if h.Status != StatusDown || h.Ready {
		t.Fatalf("got status %s ready %v", h.Status, h.Ready)
	}
}

func TestResetAllClearsEverythingAndRecyclesRuntime(t *testing.T) {
	f := newFixture("s3-objects", "sqs-queues", "lambda-functions")
	res, err := f.svc.Reset(context.Background(), nil)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(f.state.buckets) != 0 {
		t.Fatalf("buckets left: %v", f.state.buckets)
	}
	if f.blobs.clearCalls != 1 || f.runtime.resets != 1 || !res.RuntimeRecycled {
		t.Fatalf("expected blobs cleared and runtime recycled: %+v %+v", f.blobs, res)
	}
	if !reflect.DeepEqual(res.Services, DefaultCatalog().Names()) {
		t.Fatalf("services: %v", res.Services)
	}
}

func TestResetOneServiceLeavesOthersAlone(t *testing.T) {
	f := newFixture("s3-objects", "s3-buckets", "sqs-queues", "dynamodb-tables", "dynamodb-streams")
	res, err := f.svc.Reset(context.Background(), []string{"s3", "dynamodb"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	cleared := append([]string(nil), res.BucketsCleared...)
	sort.Strings(cleared)
	if want := []string{"dynamodb-tables", "s3-buckets", "s3-objects"}; !reflect.DeepEqual(cleared, want) {
		t.Fatalf("cleared %v, want %v", cleared, want)
	}
	if !f.state.buckets["sqs-queues"] || !f.state.buckets["dynamodb-streams"] {
		t.Fatalf("unrelated buckets were cleared: %v", f.state.buckets)
	}
	if !reflect.DeepEqual(f.blobs.deletedNS, []string{"s3"}) || f.blobs.clearCalls != 0 {
		t.Fatalf("blob namespaces: %v clear=%d", f.blobs.deletedNS, f.blobs.clearCalls)
	}
	if f.runtime.resets != 0 || res.RuntimeRecycled {
		t.Fatal("runtime should not be recycled for s3/dynamodb")
	}
}

func TestResetLambdaRecyclesRuntimeAndClearsCodeAndLayers(t *testing.T) {
	f := newFixture("lambda-functions", "lambda-layers")
	res, err := f.svc.Reset(context.Background(), []string{"lambda"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !res.RuntimeRecycled || f.runtime.resets != 1 {
		t.Fatal("expected runtime recycle")
	}
	if !reflect.DeepEqual(f.blobs.deletedNS, []string{"lambda", "lambda-layers"}) {
		t.Fatalf("blob namespaces: %v", f.blobs.deletedNS)
	}
}

func TestResetRejectsUnknownServiceWithoutTouchingState(t *testing.T) {
	f := newFixture("s3-objects")
	_, err := f.svc.Reset(context.Background(), []string{"s4"})
	requireCode(t, err, "UnknownService")
	if !f.state.buckets["s3-objects"] {
		t.Fatal("state was modified")
	}
}

func TestCreateSnapshotWithNameRecordsServicesWithState(t *testing.T) {
	f := newFixture("s3-objects", "sqs-queues", "sqs-messages")
	snap, err := f.svc.CreateSnapshot(context.Background(), "baseline")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if snap.ID != "baseline" || !snap.CreatedAt.Equal(t0.Add(90*time.Second)) {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if want := []string{"s3", "sqs"}; !reflect.DeepEqual(snap.Services, want) {
		t.Fatalf("services: %v, want %v", snap.Services, want)
	}
	paths := fakePaths("baseline")
	if !reflect.DeepEqual(f.state.written, []string{paths.MetaDB}) || !reflect.DeepEqual(f.blobs.cloned, []string{paths.BlobDir}) {
		t.Fatalf("wrote %v / cloned %v", f.state.written, f.blobs.cloned)
	}
}

func TestCreateSnapshotGeneratesNameWhenEmpty(t *testing.T) {
	snap, err := newFixture().svc.CreateSnapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(string(snap.ID), "snap-20261104t080130-") {
		t.Fatalf("unexpected generated id %q", snap.ID)
	}
	if _, err := ParseSnapshotID(string(snap.ID)); err != nil {
		t.Fatalf("generated id is not valid: %v", err)
	}
}

func TestCreateSnapshotRejectsInvalidAndDuplicateNames(t *testing.T) {
	f := newFixture()
	for _, bad := range []string{"../etc", "UPPER", "-lead", "a/b", strings.Repeat("x", 64)} {
		_, err := f.svc.CreateSnapshot(context.Background(), bad)
		requireCode(t, err, "InvalidSnapshotName")
	}
	if _, err := f.svc.CreateSnapshot(context.Background(), "baseline"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := f.svc.CreateSnapshot(context.Background(), "baseline")
	requireCode(t, err, "SnapshotAlreadyExists")
}

func TestCreateSnapshotAbortsStagingWhenCopyFails(t *testing.T) {
	f := newFixture()
	f.blobs.cloneErr = errBoom
	_, err := f.svc.CreateSnapshot(context.Background(), "broken")
	requireCode(t, err, "InternalError")
	if !reflect.DeepEqual(f.repo.aborted, []SnapshotID{"broken"}) {
		t.Fatalf("staging not aborted: %v", f.repo.aborted)
	}
	if _, _, err := f.repo.Get("broken"); err == nil {
		t.Fatal("failed snapshot must not be committed")
	}
}

func TestCreateSnapshotKeepsDomainErrorFromCommit(t *testing.T) {
	f := newFixture()
	f.repo.commitErr = snapshotExists("race")
	_, err := f.svc.CreateSnapshot(context.Background(), "race")
	requireCode(t, err, "SnapshotAlreadyExists")
}

func TestRestoreSnapshotRestoresStateBlobsAndRecyclesRuntime(t *testing.T) {
	f := newFixture()
	if _, err := f.svc.CreateSnapshot(context.Background(), "baseline"); err != nil {
		t.Fatalf("create: %v", err)
	}
	snap, err := f.svc.RestoreSnapshot(context.Background(), "baseline")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	paths := fakePaths("baseline")
	if snap.ID != "baseline" ||
		!reflect.DeepEqual(f.state.restored, []string{paths.MetaDB}) ||
		!reflect.DeepEqual(f.blobs.replaced, []string{paths.BlobDir}) ||
		f.runtime.resets != 1 {
		t.Fatalf("restore did not touch all adapters: %+v %+v %+v", f.state, f.blobs, f.runtime)
	}
}

func TestRestoreUnknownSnapshotReturnsNotFound(t *testing.T) {
	_, err := newFixture().svc.RestoreSnapshot(context.Background(), "nope")
	requireCode(t, err, "SnapshotNotFound")
}

func TestRestoreStopsWhenMetadataRestoreFails(t *testing.T) {
	f := newFixture()
	_, _ = f.svc.CreateSnapshot(context.Background(), "baseline")
	f.state.restoreFn = func(string) error { return errBoom }
	_, err := f.svc.RestoreSnapshot(context.Background(), "baseline")
	requireCode(t, err, "InternalError")
	if len(f.blobs.replaced) != 0 || f.runtime.resets != 0 {
		t.Fatal("later steps ran after a failed metadata restore")
	}
}

func TestListSnapshotsOrdersOldestFirst(t *testing.T) {
	f := newFixture()
	f.repo.snaps["b"] = Snapshot{ID: "b", CreatedAt: t0.Add(time.Minute)}
	f.repo.snaps["a"] = Snapshot{ID: "a", CreatedAt: t0}
	f.repo.snaps["c"] = Snapshot{ID: "c", CreatedAt: t0.Add(time.Minute)}
	snaps, err := f.svc.ListSnapshots()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []SnapshotID
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	if want := []SnapshotID{"a", "b", "c"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
}

func TestDeleteSnapshot(t *testing.T) {
	f := newFixture()
	_, _ = f.svc.CreateSnapshot(context.Background(), "baseline")
	if err := f.svc.DeleteSnapshot("baseline"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	requireCode(t, f.svc.DeleteSnapshot("baseline"), "SnapshotNotFound")
}
