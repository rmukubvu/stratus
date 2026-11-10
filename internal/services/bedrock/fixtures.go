package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stratus/internal/store"
)

const (
	fixturesBucket = "bedrock-fixtures"
	// OriginRuntime marks fixtures added through the control API. They live
	// in the metadata store, so reset clears them and snapshots keep them.
	OriginRuntime = "runtime"
)

// runtimeFixtures are fixtures added through the control API, kept in
// insertion order.
type runtimeFixtures struct {
	metadata store.Store
}

func runtimeKey(created time.Time, id string) string {
	return fmt.Sprintf("%020d/%s", created.UnixNano(), id)
}

func (r runtimeFixtures) list() ([]Fixture, error) {
	var out []Fixture
	err := r.metadata.Scan(fixturesBucket, "", func(_, v []byte) error {
		var f Fixture
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		f.Origin = OriginRuntime
		out = append(out, f)
		return nil
	})
	return out, err
}

func (r runtimeFixtures) add(f Fixture, now time.Time) error {
	existing, err := r.list()
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.ID == f.ID {
			return &conflictError{id: f.ID}
		}
	}
	f.Origin = ""
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return r.metadata.Put(fixturesBucket, runtimeKey(now, f.ID), raw)
}

func (r runtimeFixtures) delete(id string) (bool, error) {
	var key string
	err := r.metadata.Scan(fixturesBucket, "", func(k, _ []byte) error {
		if strings.HasSuffix(string(k), "/"+id) {
			key = string(k)
		}
		return nil
	})
	if err != nil || key == "" {
		return false, err
	}
	return true, r.metadata.Delete(fixturesBucket, key)
}

func (r runtimeFixtures) clear() (int, error) {
	existing, err := r.list()
	if err != nil {
		return 0, err
	}
	return len(existing), r.metadata.DeletePrefix(fixturesBucket, "")
}

type conflictError struct{ id string }

func (e *conflictError) Error() string {
	return fmt.Sprintf("a fixture with id %q already exists", e.id)
}

// fixtureDir is a directory of fixture files, meant to be committed next to
// the tests that use them. Each *.json file holds one fixture or an array
// of them; subdirectories are read too, in path order.
type fixtureDir struct {
	path string
}

func (d fixtureDir) list() ([]Fixture, error) {
	if d.path == "" {
		return nil, nil
	}
	var files []string
	err := filepath.WalkDir(d.path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, p)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read fixture directory %s: %w", d.path, err)
	}
	sort.Strings(files)

	var out []Fixture
	for _, file := range files {
		fixtures, err := readFixtureFile(file)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(d.path, file)
		rel = filepath.ToSlash(rel)
		base := strings.TrimSuffix(filepath.Base(file), ".json")
		for i, f := range fixtures {
			if f.ID == "" {
				f.ID = base
				if len(fixtures) > 1 {
					f.ID = fmt.Sprintf("%s-%d", base, i+1)
				}
			}
			if err := f.Validate(); err != nil {
				return nil, fmt.Errorf("fixture file %s: %s: %w", rel, f.ID, err)
			}
			f.Origin = rel
			out = append(out, f)
		}
	}
	return out, nil
}

func readFixtureFile(file string) ([]Fixture, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var fixtures []Fixture
		if err := json.Unmarshal(raw, &fixtures); err != nil {
			return nil, fmt.Errorf("fixture file %s: %w", file, err)
		}
		return fixtures, nil
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("fixture file %s: %w", file, err)
	}
	return []Fixture{f}, nil
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// recordedID names a recorded fixture after its kind, model and hash, so
// the file is easy to find and re-recording overwrites it.
func recordedID(f Fixture) string {
	model := strings.Trim(unsafeFileChars.ReplaceAllString(BaseModelID(f.Model), "_"), "_")
	return fmt.Sprintf("%s-%s-%s", f.Kind, model, strings.TrimPrefix(f.RequestHash, hashPrefix)[:12])
}

// save writes a recorded fixture to <id>.json and returns the file's path.
func (d fixtureDir) save(f Fixture) (string, error) {
	if err := os.MkdirAll(d.path, 0o755); err != nil {
		return "", err
	}
	f.Origin = ""
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(d.path, f.ID+".json")
	tmp, err := os.CreateTemp(d.path, ".recording-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}
