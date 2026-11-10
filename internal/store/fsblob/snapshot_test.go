package fsblob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putBlob(t *testing.T, s *Store, ns, key, body string) {
	t.Helper()
	if _, err := s.Put(ns, key, strings.NewReader(body)); err != nil {
		t.Fatalf("put %s/%s: %v", ns, key, err)
	}
}

func readBlob(t *testing.T, s *Store, ns, key string) (string, bool) {
	t.Helper()
	f, err := s.Open(ns, key)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("open %s/%s: %v", ns, key, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b), true
}

func TestClearRemovesAllBlobsButKeepsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	s := New(root)
	putBlob(t, s, "s3/assets", "a.txt", "hello")

	if err := s.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := readBlob(t, s, "s3/assets", "a.txt"); ok {
		t.Fatal("blob survived clear")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("root missing after clear: %v", err)
	}
}

func TestCloneAndReplaceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "blobs"))
	putBlob(t, s, "s3/assets", "a.txt", "v1")
	putBlob(t, s, "lambda/fn", "code.zip", "zip")

	snap := filepath.Join(dir, "snap")
	if err := s.CloneTo(snap); err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Overwrite, add and delete after the snapshot.
	putBlob(t, s, "s3/assets", "a.txt", "v2")
	putBlob(t, s, "s3/assets", "b.txt", "new")
	if err := s.DeleteNamespace("lambda/fn"); err != nil {
		t.Fatalf("delete ns: %v", err)
	}

	// The snapshot must not see the overwrite (hard links stay immutable).
	snapBody, err := os.ReadFile(filepath.Join(snap, "s3", "assets", "a.txt"))
	if err != nil || string(snapBody) != "v1" {
		t.Fatalf("snapshot mutated: %q, %v", snapBody, err)
	}

	if err := s.ReplaceWith(snap); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := readBlob(t, s, "s3/assets", "a.txt"); got != "v1" {
		t.Fatalf("a.txt: got %q, want v1", got)
	}
	if _, ok := readBlob(t, s, "s3/assets", "b.txt"); ok {
		t.Fatal("b.txt should be gone after restore")
	}
	if got, _ := readBlob(t, s, "lambda/fn", "code.zip"); got != "zip" {
		t.Fatalf("code.zip: got %q", got)
	}

	// Restoring must not consume the snapshot: it can be restored again.
	putBlob(t, s, "s3/assets", "a.txt", "v3")
	if err := s.ReplaceWith(snap); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	if got, _ := readBlob(t, s, "s3/assets", "a.txt"); got != "v1" {
		t.Fatalf("second restore: got %q", got)
	}
}

func TestCloneToRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "blobs"))
	if err := s.CloneTo(dir); err == nil {
		t.Fatal("expected error when destination exists")
	}
}

func TestReplaceWithMissingSourceKeepsLiveBlobs(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "blobs"))
	putBlob(t, s, "s3/assets", "a.txt", "v1")

	if err := s.ReplaceWith(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing source")
	}
	if got, _ := readBlob(t, s, "s3/assets", "a.txt"); got != "v1" {
		t.Fatalf("live blobs changed: %q", got)
	}
}

func TestDeleteNamespaceIncludesScopedStores(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	scoped := s.Scoped("@111122223333/eu-west-1/")
	for _, st := range []*Store{s, scoped} {
		if _, err := st.Put("lambda", "fn/code.zip", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Put("s3", "bucket/key", strings.NewReader("y")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteNamespace("lambda"); err != nil {
		t.Fatal(err)
	}
	for _, st := range []*Store{s, scoped} {
		if _, err := os.Stat(st.NamespacePath("lambda")); !os.IsNotExist(err) {
			t.Fatalf("lambda namespace survived in %s", st.NamespacePath(""))
		}
		if _, err := os.Stat(st.NamespacePath("s3")); err != nil {
			t.Fatalf("s3 namespace was removed from %s", st.NamespacePath(""))
		}
	}
}
