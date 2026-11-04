package control

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSRepositoryStageCommitGetListDelete(t *testing.T) {
	root := t.TempDir()
	repo, err := NewFSSnapshotRepository(root)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}

	staging, err := repo.Stage("baseline")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := os.WriteFile(staging.Paths().MetaDB, []byte("db"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Not visible before commit.
	if _, _, err := repo.Get("baseline"); err == nil {
		t.Fatal("snapshot visible before commit")
	}

	snap := Snapshot{ID: "baseline", CreatedAt: time.Unix(0, 0).UTC(), Services: []string{"s3"}}
	if err := staging.Commit(snap); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, paths, err := repo.Get("baseline")
	if err != nil || got.ID != "baseline" || got.Services[0] != "s3" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if raw, err := os.ReadFile(paths.MetaDB); err != nil || string(raw) != "db" {
		t.Fatalf("meta db not moved into place: %q %v", raw, err)
	}

	list, err := repo.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	if _, err := repo.Stage("baseline"); err == nil {
		t.Fatal("expected conflict staging an existing id")
	} else {
		requireCode(t, err, "SnapshotAlreadyExists")
	}

	if err := repo.Delete("baseline"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	requireCode(t, repo.Delete("baseline"), "SnapshotNotFound")
}

func TestFSRepositoryAbortLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	repo, _ := NewFSSnapshotRepository(root)
	staging, _ := repo.Stage("tmp")
	if err := staging.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("leftovers after abort: %v", entries)
	}
}

func TestFSRepositoryCleansStaleStagingOnStartup(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, stagingPrefix+"crashed-123")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFSSnapshotRepository(root); err != nil {
		t.Fatalf("new repo: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale staging dir was not removed")
	}
}
