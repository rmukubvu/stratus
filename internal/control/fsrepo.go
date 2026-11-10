package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	manifestFile   = "manifest.json"
	metaDBFile     = "meta.db"
	blobDirName    = "blobs"
	engineDirName  = "engines"
	stagingPrefix  = ".staging-"
	manifestFormat = 1
)

type manifest struct {
	Format   int      `json:"format"`
	Snapshot Snapshot `json:"snapshot"`
}

// FSSnapshotRepository stores each snapshot as a directory:
//
//	<root>/<id>/manifest.json
//	<root>/<id>/meta.db
//	<root>/<id>/blobs/...
//	<root>/<id>/engines/<service>/<resource>.tar.gz
//
// A snapshot is written to a hidden staging directory and renamed into place
// on commit, so readers never see a partial snapshot.
type FSSnapshotRepository struct {
	root string
}

// NewFSSnapshotRepository creates the root directory if needed and removes
// staging directories left behind by an interrupted snapshot.
func NewFSSnapshotRepository(root string) (*FSSnapshotRepository, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read snapshot dir: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	return &FSSnapshotRepository{root: root}, nil
}

func pathsIn(dir string) SnapshotPaths {
	return SnapshotPaths{
		MetaDB:    filepath.Join(dir, metaDBFile),
		BlobDir:   filepath.Join(dir, blobDirName),
		EngineDir: filepath.Join(dir, engineDirName),
	}
}

func (r *FSSnapshotRepository) dir(id SnapshotID) string {
	return filepath.Join(r.root, string(id))
}

// Stage reserves a staging directory for a new snapshot.
func (r *FSSnapshotRepository) Stage(id SnapshotID) (SnapshotStaging, error) {
	if _, err := os.Stat(r.dir(id)); err == nil {
		return nil, snapshotExists(id)
	}
	dir, err := os.MkdirTemp(r.root, stagingPrefix+string(id)+"-")
	if err != nil {
		return nil, internalError("create snapshot staging dir", err)
	}
	return &fsStaging{dir: dir, final: r.dir(id), id: id}, nil
}

// Get loads a committed snapshot.
func (r *FSSnapshotRepository) Get(id SnapshotID) (Snapshot, SnapshotPaths, error) {
	dir := r.dir(id)
	m, err := readManifest(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, SnapshotPaths{}, snapshotNotFound(id)
		}
		return Snapshot{}, SnapshotPaths{}, internalError("read snapshot manifest", err)
	}
	return m.Snapshot, pathsIn(dir), nil
}

// List returns every committed snapshot. Unreadable entries are skipped.
func (r *FSSnapshotRepository) List() ([]Snapshot, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, err
	}
	snaps := make([]Snapshot, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m, err := readManifest(filepath.Join(r.root, e.Name()))
		if err != nil {
			continue
		}
		snaps = append(snaps, m.Snapshot)
	}
	return snaps, nil
}

// Delete removes a committed snapshot.
func (r *FSSnapshotRepository) Delete(id SnapshotID) error {
	dir := r.dir(id)
	if _, err := os.Stat(filepath.Join(dir, manifestFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshotNotFound(id)
		}
		return internalError("stat snapshot", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return internalError("delete snapshot", err)
	}
	return nil
}

func readManifest(dir string) (manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Format != manifestFormat {
		return manifest{}, fmt.Errorf("unsupported snapshot format %d", m.Format)
	}
	return m, nil
}

type fsStaging struct {
	dir   string
	final string
	id    SnapshotID
}

func (s *fsStaging) Paths() SnapshotPaths { return pathsIn(s.dir) }

func (s *fsStaging) Commit(snap Snapshot) error {
	raw, err := json.MarshalIndent(manifest{Format: manifestFormat, Snapshot: snap}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, manifestFile), raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(s.dir, s.final); err != nil {
		if _, statErr := os.Stat(s.final); statErr == nil {
			return snapshotExists(s.id)
		}
		return err
	}
	return nil
}

func (s *fsStaging) Abort() error {
	return os.RemoveAll(s.dir)
}
