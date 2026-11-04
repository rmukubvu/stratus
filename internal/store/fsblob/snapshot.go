package fsblob

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Blob files are written to a temp file and renamed into place, and never
// modified afterwards. That makes it safe to share them between the live
// store and snapshots via hard links: a snapshot is close to free in time and
// disk, and later writes to the live store replace files instead of mutating
// them, so snapshots are never affected.

const tempBlobPrefix = ".blob-"

// Clear removes every blob in the store, leaving an empty root directory.
func (s *Store) Clear() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(s.root, 0o755)
		}
		return fmt.Errorf("read blob root: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
			return fmt.Errorf("remove %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// CloneTo copies the full blob tree into dst, which must not exist yet.
// Files are hard-linked where the filesystem allows it and copied otherwise.
func (s *Store) CloneTo(dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("clone blobs: destination %q already exists", dst)
	}
	if _, err := os.Stat(s.root); os.IsNotExist(err) {
		return os.MkdirAll(dst, 0o755)
	}
	return cloneTree(s.root, dst)
}

// ReplaceWith makes the store's contents identical to the tree at src.
// The new tree is staged next to the root and swapped in with renames, so a
// failure while staging leaves the live store untouched.
func (s *Store) ReplaceWith(src string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("replace blobs: %w", err)
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	staging := s.root + ".restore-" + stamp
	if err := cloneTree(src, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("stage blobs: %w", err)
	}

	retired := s.root + ".retired-" + stamp
	hadRoot := true
	if err := os.Rename(s.root, retired); err != nil {
		if !os.IsNotExist(err) {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("retire current blobs: %w", err)
		}
		hadRoot = false
	}
	if err := os.Rename(staging, s.root); err != nil {
		if hadRoot {
			_ = os.Rename(retired, s.root)
		}
		_ = os.RemoveAll(staging)
		return fmt.Errorf("activate restored blobs: %w", err)
	}
	if hadRoot {
		if err := os.RemoveAll(retired); err != nil {
			return fmt.Errorf("remove retired blobs: %w", err)
		}
	}
	return nil
}

// CloneTree copies the blob tree at src into dst, hard-linking files where
// the filesystem allows. It is used to seed a fresh data directory.
func CloneTree(src, dst string) error {
	return cloneTree(src, dst)
}

func cloneTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if strings.HasPrefix(d.Name(), tempBlobPrefix) || !d.Type().IsRegular() {
			return nil
		}
		return linkOrCopy(path, target)
	})
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
