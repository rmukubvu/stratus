package control_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/control"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

// makeSnapshot builds a real snapshot through the control service.
func makeSnapshot(t *testing.T) (snapshotDir string) {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	blobs := fsblob.New(filepath.Join(dir, "blobs"))
	repo, err := control.NewFSSnapshotRepository(filepath.Join(dir, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	svc := control.NewService(control.Options{Catalog: control.DefaultCatalog(), State: meta, Blobs: blobs, Snapshots: repo})

	if err := meta.Put("sqs-queues", "orders", []byte("seeded")); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put("s3/assets", "logo.png", strings.NewReader("png")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSnapshot(t.Context(), "seeded"); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "snapshots", "seeded")
}

func TestSeedDataDirGivesEachRunAnIndependentCopy(t *testing.T) {
	snapDir := makeSnapshot(t)
	runA, runB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")

	for _, dir := range []string{runA, runB} {
		snap, err := control.SeedDataDir(snapDir, dir)
		if err != nil || snap.ID != "seeded" {
			t.Fatalf("seed %s: %+v %v", dir, snap, err)
		}
	}

	metaA, err := bbolt.Open(filepath.Join(runA, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metaA.Close()
	if v, _ := metaA.Get("sqs-queues", "orders"); string(v) != "seeded" {
		t.Fatalf("seeded metadata missing: %q", v)
	}
	// Run A writes; run B and the snapshot must not see it.
	if err := metaA.Put("sqs-queues", "orders", []byte("changed-by-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsblob.New(filepath.Join(runA, "blobs")).Put("s3/assets", "logo.png", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}

	metaB, err := bbolt.Open(filepath.Join(runB, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metaB.Close()
	if v, _ := metaB.Get("sqs-queues", "orders"); string(v) != "seeded" {
		t.Fatalf("run B saw run A's write: %q", v)
	}
	for _, path := range []string{
		filepath.Join(runB, "blobs", "s3", "assets", "logo.png"),
		filepath.Join(snapDir, "blobs", "s3", "assets", "logo.png"),
	} {
		if raw, _ := os.ReadFile(path); string(raw) != "png" {
			t.Fatalf("%s changed: %q", path, raw)
		}
	}
}

func TestSeedDataDirRefusesNonEmptyDataDir(t *testing.T) {
	snapDir := makeSnapshot(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "meta.db"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := control.SeedDataDir(snapDir, dataDir)
	if ce := control.AsError(err); ce.Code != "DataDirNotEmpty" {
		t.Fatalf("got %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dataDir, "meta.db")); string(raw) != "live" {
		t.Fatal("existing state was overwritten")
	}
}

func TestSeedDataDirRejectsNonSnapshotDir(t *testing.T) {
	_, err := control.SeedDataDir(t.TempDir(), t.TempDir())
	if ce := control.AsError(err); ce.Code != "InvalidSeedSnapshot" {
		t.Fatalf("got %v", err)
	}
}
