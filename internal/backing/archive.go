package backing

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// archiveExt is the file extension of an engine data archive: a gzip
// compressed tar of the engine's data directory.
const archiveExt = ".tar.gz"

// ArchivePath is where the archive for the engine name lives in dir.
func ArchivePath(dir, name string) string {
	return filepath.Join(dir, name+archiveExt)
}

// SeedFrom returns the archive for the engine name in dir, or "" when dir is
// empty or holds no archive for it. A snapshot taken before engine data was
// captured has none, and its engines start empty.
func SeedFrom(dir, name string) string {
	if dir == "" {
		return ""
	}
	path := ArchivePath(dir, name)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// WriteArchive stores the tar stream src as the archive dst. The stream is a
// copy of one data directory, so every entry sits under a single top-level
// directory; entries directly under it whose names appear in exclude are
// dropped. The archive appears at dst only once it is complete.
func WriteArchive(dst string, src io.Reader, exclude []string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-")
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	zw, err := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if err := copyTar(tar.NewWriter(zw), tar.NewReader(src), toSet(exclude)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("commit archive: %w", err)
	}
	return nil
}

func copyTar(tw *tar.Writer, tr *tar.Reader, exclude map[string]struct{}) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read engine data: %w", err)
		}
		if _, skip := exclude[childName(hdr.Name)]; skip {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write archive entry %s: %w", hdr.Name, err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("write archive entry %s: %w", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	return nil
}

// childName returns the part of an entry name below the top-level directory,
// so "data/postmaster.pid" becomes "postmaster.pid".
func childName(name string) string {
	name = strings.TrimPrefix(name, "./")
	_, rest, found := strings.Cut(name, "/")
	if !found {
		return ""
	}
	return strings.TrimSuffix(rest, "/")
}

// OpenArchive opens an archive written by WriteArchive and returns its tar
// stream.
func OpenArchive(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open archive %s: %w", path, err)
	}
	return &archiveReader{Reader: zr, file: f}, nil
}

type archiveReader struct {
	*gzip.Reader
	file *os.File
}

func (r *archiveReader) Close() error {
	return errors.Join(r.Reader.Close(), r.file.Close())
}

func toSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}
