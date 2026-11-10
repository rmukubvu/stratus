package container

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMergeLayersLaterLayerWins(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "l1")
	second := filepath.Join(root, "l2")
	writeFile(t, filepath.Join(first, "python", "shared.py"), "v1")
	writeFile(t, filepath.Join(first, "python", "only_first.py"), "first")
	writeFile(t, filepath.Join(second, "python", "shared.py"), "v2")

	dst := filepath.Join(root, "opt")
	writeFile(t, filepath.Join(dst, "stale.txt"), "old")
	if err := mergeLayers(dst, []string{first, second}); err != nil {
		t.Fatal(err)
	}

	assertBody := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}
	assertBody("python/shared.py", "v2")
	assertBody("python/only_first.py", "first")
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale files to be cleared")
	}
}

func TestExtractSingleFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "aws-lambda-rie", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bin"))
	_ = tw.Close()

	dst := filepath.Join(t.TempDir(), "rie", "aws-lambda-rie")
	if err := extractSingleFile(&buf, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "bin" {
		t.Fatalf("extracted %q, %v", got, err)
	}
	if !isRegularFile(dst) {
		t.Fatalf("isRegularFile(%s) = false", dst)
	}
}
