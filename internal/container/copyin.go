package container

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/docker/docker/api/types/mount"
)

// fileMode is the mode a copied file gets in the container. Windows has no
// executable bit to read, so every file is made executable there, as Docker
// Desktop's bind mounts also present them, and a bootstrap still runs.
func fileMode(mode int64) int64 {
	if runtime.GOOS == "windows" {
		return 0o755
	}
	return mode
}

// splitBinds separates the bind mounts a launch plan asks for, which must be
// copied in when the daemon cannot see Stratus's filesystem, from the mounts
// that can stay as they are.
func splitBinds(mounts []mount.Mount) (keep, copies []mount.Mount) {
	for _, m := range mounts {
		if m.Type == mount.TypeBind {
			copies = append(copies, m)
			continue
		}
		keep = append(keep, m)
	}
	return keep, copies
}

// archiveMounts writes a tar stream that, unpacked at the container's root,
// puts each bind source at its target: a directory's contents under the
// target directory, a single file at the target path.
func archiveMounts(w io.Writer, mounts []mount.Mount) error {
	tw := tar.NewWriter(w)
	written := map[string]bool{}
	for _, m := range mounts {
		target := strings.TrimPrefix(path.Clean("/"+m.Target), "/")
		if target == "" {
			return fmt.Errorf("cannot copy %s over the container root", m.Source)
		}
		info, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("copy %s: %w", m.Source, err)
		}
		dir := path.Dir(target)
		if info.IsDir() {
			dir = target
		}
		if err := writeParents(tw, dir, written); err != nil {
			return err
		}
		if info.IsDir() {
			err = archiveDir(tw, m.Source, target, written)
		} else {
			err = archiveFile(tw, m.Source, target, info)
		}
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeParents adds root-owned 0755 directory entries for dir and its
// parents, which is what /var, /opt and /usr/local already are in Lambda
// images, so unpacking over them changes nothing.
func writeParents(tw *tar.Writer, dir string, written map[string]bool) error {
	if dir == "." || dir == "" || written[dir] {
		return nil
	}
	if err := writeParents(tw, path.Dir(dir), written); err != nil {
		return err
	}
	written[dir] = true
	return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755})
}

func archiveDir(tw *tar.Writer, root, target string, written map[string]bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := path.Join(target, filepath.ToSlash(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			return tw.WriteHeader(hdr)
		case info.IsDir():
			if written[name] {
				return nil
			}
			written[name] = true
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			return archiveFile(tw, p, name, info)
		default:
			return nil // sockets, devices and pipes have no place in a function
		}
	})
}

func archiveFile(tw *tar.Writer, src, name string, info fs.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.Mode = fileMode(hdr.Mode)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
