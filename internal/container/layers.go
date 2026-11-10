package container

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// mergeLayers copies layer directories into dst in order, so a later layer's
// file replaces an earlier one's: the /opt Lambda builds from a function's
// layers. dst is emptied first.
func mergeLayers(dst string, layerDirs []string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clear merged layers: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create merged layers: %w", err)
	}
	for _, src := range layerDirs {
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("merge layer %s: %w", src, err)
		}
	}
	return nil
}

func copyTree(src, dst string) error {
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
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// isRegularFile reports whether path is a regular file. Execute bits are
// not checked, because Windows hosts do not keep them; extraction already
// marks bootstrap executable.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// extractSingleFile writes the first regular file in a tar stream to dst,
// executable. Docker's copy API returns files as tar archives.
func extractSingleFile(archive io.Reader, dst string) error {
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return fmt.Errorf("archive holds no regular file")
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	}
}
