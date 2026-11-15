package container

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestSplitBindsCopiesOnlyBindMounts(t *testing.T) {
	code := readOnlyBind("/data/code", taskRoot)
	tmp := mount.Mount{Type: mount.TypeTmpfs, Target: "/tmp"}
	keep, copies := splitBinds([]mount.Mount{code, tmp})
	if !reflect.DeepEqual(keep, []mount.Mount{tmp}) || !reflect.DeepEqual(copies, []mount.Mount{code}) {
		t.Fatalf("keep %v copies %v", keep, copies)
	}
}

type entry struct {
	typ  byte
	mode int64
	body string
	link string
}

func untar(t *testing.T, data []byte) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[hdr.Name] = entry{typ: hdr.Typeflag, mode: hdr.Mode & 0o777, body: string(body), link: hdr.Linkname}
	}
}

func TestArchiveMountsPlacesSourcesAtTheirTargets(t *testing.T) {
	code := t.TempDir()
	write(t, filepath.Join(code, "app.py"), "def handler(e, c): return 1", 0o644)
	write(t, filepath.Join(code, "bootstrap"), "#!/bin/sh", 0o755)
	write(t, filepath.Join(code, "lib", "util.py"), "x = 1", 0o644)
	if err := os.Symlink("app.py", filepath.Join(code, "main.py")); err != nil {
		t.Fatal(err)
	}
	rie := filepath.Join(t.TempDir(), "aws-lambda-rie")
	write(t, rie, "ELF", 0o755)

	var buf bytes.Buffer
	err := archiveMounts(&buf, []mount.Mount{
		readOnlyBind(code, taskRoot),
		{Type: mount.TypeBind, Source: rie, Target: mountedRIEPath, ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, buf.Bytes())

	want := map[string]entry{
		"var/":                             {typ: tar.TypeDir, mode: 0o755},
		"var/task/":                        {typ: tar.TypeDir, mode: 0o755},
		"var/task/app.py":                  {typ: tar.TypeReg, mode: 0o644, body: "def handler(e, c): return 1"},
		"var/task/bootstrap":               {typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh"},
		"var/task/lib/util.py":             {typ: tar.TypeReg, mode: 0o644, body: "x = 1"},
		"var/task/main.py":                 {typ: tar.TypeSymlink, mode: 0o777, link: "app.py"},
		"usr/":                             {typ: tar.TypeDir, mode: 0o755},
		"usr/local/":                       {typ: tar.TypeDir, mode: 0o755},
		"usr/local/stratus/":               {typ: tar.TypeDir, mode: 0o755},
		"usr/local/stratus/aws-lambda-rie": {typ: tar.TypeReg, mode: 0o755, body: "ELF"},
	}
	for name, w := range want {
		if runtime.GOOS == "windows" && w.typ == tar.TypeReg {
			w.mode = 0o755 // no executable bit to read on Windows
		}
		g, ok := got[name]
		if runtime.GOOS == "windows" && w.typ == tar.TypeSymlink {
			g.mode = w.mode // symlink modes are not meaningful on Windows
		}
		if !ok || g != w {
			t.Errorf("%s: got %+v (present %v), want %+v", name, g, ok, w)
		}
	}
	if _, ok := got["var/task/lib/"]; !ok {
		t.Error("subdirectory entry missing")
	}
}

func TestArchiveMountsRefusesTheRoot(t *testing.T) {
	if err := archiveMounts(io.Discard, []mount.Mount{readOnlyBind(t.TempDir(), "/")}); err == nil {
		t.Fatal("copied over the container root")
	}
}

func TestArchiveMountsReportsAMissingSource(t *testing.T) {
	if err := archiveMounts(io.Discard, []mount.Mount{readOnlyBind("/nonexistent/stratus", taskRoot)}); err == nil {
		t.Fatal("want an error")
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
