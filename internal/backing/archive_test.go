package backing

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name string
	body string
	dir  bool
}

func tarOf(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: tar.TypeReg, Uid: 70, Gid: 70}
		if e.dir {
			hdr = &tar.Header{Name: e.name, Mode: 0o700, Typeflag: tar.TypeDir, Uid: 70, Gid: 70}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func readArchive(t *testing.T, path string) map[string]*tar.Header {
	t.Helper()
	rc, err := OpenArchive(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer rc.Close()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		out[hdr.Name] = hdr
	}
}

func TestWriteArchiveRoundTripsEntriesWithOwnership(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "rds", "orders-db.tar.gz")
	src := tarOf(t,
		entry{name: "data/", dir: true},
		entry{name: "data/PG_VERSION", body: "16\n"},
		entry{name: "data/base/", dir: true},
		entry{name: "data/base/1", body: "rows"},
	)

	if err := WriteArchive(dst, src, nil); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	got := readArchive(t, dst)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %v", got)
	}
	if hdr := got["data/PG_VERSION"]; hdr == nil || hdr.Uid != 70 || hdr.Size != 3 {
		t.Fatalf("file entry should keep its size and owner: %+v", hdr)
	}
}

func TestWriteArchiveDropsExcludedTopLevelEntriesOnly(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "db.tar.gz")
	src := tarOf(t,
		entry{name: "data/", dir: true},
		entry{name: "data/postmaster.pid", body: "1"},
		entry{name: "data/base/", dir: true},
		entry{name: "data/base/postmaster.pid", body: "kept"},
	)

	if err := WriteArchive(dst, src, []string{"postmaster.pid"}); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	got := readArchive(t, dst)
	if _, ok := got["data/postmaster.pid"]; ok {
		t.Fatal("excluded entry should be dropped")
	}
	if _, ok := got["data/base/postmaster.pid"]; !ok {
		t.Fatal("a nested entry with the same name should be kept")
	}
	if _, ok := got["data/"]; !ok {
		t.Fatal("the top-level directory should be kept")
	}
}

func TestWriteArchiveLeavesNothingBehindOnCorruptInput(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "db.tar.gz")
	src := tarOf(t, entry{name: "data/file", body: "0123456789"})
	truncated := bytes.NewReader(src.Bytes()[:515])

	if err := WriteArchive(dst, truncated, nil); err == nil {
		t.Fatal("a truncated stream should fail")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("a failed write should leave no files, found %v", entries)
	}
}

func TestSeedFromFindsOnlyExistingArchives(t *testing.T) {
	dir := t.TempDir()
	if err := WriteArchive(ArchivePath(dir, "orders-db"), tarOf(t, entry{name: "data/", dir: true}), nil); err != nil {
		t.Fatal(err)
	}

	if got := SeedFrom(dir, "orders-db"); got != ArchivePath(dir, "orders-db") {
		t.Fatalf("expected the archive path, got %q", got)
	}
	if got := SeedFrom(dir, "missing"); got != "" {
		t.Fatalf("an engine without an archive should get no seed, got %q", got)
	}
	if got := SeedFrom("", "orders-db"); got != "" {
		t.Fatalf("no data dir should mean no seed, got %q", got)
	}
}

func TestChildName(t *testing.T) {
	cases := map[string]string{
		"data/postmaster.pid":   "postmaster.pid",
		"./data/postmaster.pid": "postmaster.pid",
		"data/base/":            "base",
		"data/base/1":           "base/1",
		"data/":                 "",
		"data":                  "",
	}
	for in, want := range cases {
		if got := childName(in); got != want {
			t.Errorf("childName(%q) = %q, want %q", in, got, want)
		}
	}
}
