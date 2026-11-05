package control

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stratus/internal/store/fsblob"
)

// SeedDataDir initialises an empty data directory from a snapshot directory,
// such as <other-data-dir>/snapshots/<name>. Many CI jobs can seed their own
// data directories from the same snapshot and then run in parallel without
// sharing any state. Blobs are hard-linked, so seeding is fast.
//
// It refuses to touch a data directory that already holds state.
func SeedDataDir(snapshotDir, dataDir string) (Snapshot, error) {
	m, err := readManifest(snapshotDir)
	if err != nil {
		return Snapshot{}, &Error{
			Kind:    KindInvalid,
			Code:    "InvalidSeedSnapshot",
			Message: fmt.Sprintf("%s is not a readable Stratus snapshot: %v", snapshotDir, err),
			Hint:    "point --seed at a snapshot directory, e.g. ./data/snapshots/baseline",
		}
	}
	target := pathsIn(dataDir)
	liveMeta := filepath.Join(dataDir, metaDBFile)
	if _, err := os.Stat(liveMeta); err == nil {
		return Snapshot{}, &Error{
			Kind:    KindConflict,
			Code:    "DataDirNotEmpty",
			Message: fmt.Sprintf("data directory %s already holds state", dataDir),
			Hint:    "seed into a new, empty --data-dir, or restore the snapshot through the control API instead",
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return Snapshot{}, internalError("create data dir", err)
	}

	src := pathsIn(snapshotDir)
	if err := copyFile(src.MetaDB, target.MetaDB); err != nil {
		return Snapshot{}, internalError("seed metadata", err)
	}
	if err := os.RemoveAll(target.BlobDir); err != nil {
		return Snapshot{}, internalError("clear blob dir", err)
	}
	if _, err := os.Stat(src.BlobDir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(target.BlobDir, 0o755); err != nil {
			return Snapshot{}, internalError("create blob dir", err)
		}
		return m.Snapshot, nil
	}
	if err := fsblob.CloneTree(src.BlobDir, target.BlobDir); err != nil {
		return Snapshot{}, internalError("seed blobs", err)
	}
	return m.Snapshot, nil
}

// copyFile copies rather than links: the metadata database is written in
// place, so sharing it with the snapshot would corrupt the snapshot.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".seed-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
