package fsblob

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Store struct {
	root string
}

type WriteResult struct {
	Size         int64
	MD5Hex       string
	SHA256Base64 string
}

func New(root string) *Store {
	return &Store{root: root}
}

func (s *Store) NamespacePath(namespace string) string {
	return filepath.Join(s.root, namespace)
}

func (s *Store) Put(namespace, key string, body io.Reader) (WriteResult, error) {
	path := filepath.Join(s.root, namespace, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("create blob dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return WriteResult{}, fmt.Errorf("create temp blob: %w", err)
	}

	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmp.Name())
		}
	}()

	md5Hash := md5.New()
	sha256Hash := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(body, io.MultiWriter(md5Hash, sha256Hash)))
	if err != nil {
		return WriteResult{}, fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("close temp blob: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return WriteResult{}, fmt.Errorf("move blob into place: %w", err)
	}
	success = true

	return WriteResult{
		Size:         size,
		MD5Hex:       hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256Base64: base64.StdEncoding.EncodeToString(sha256Hash.Sum(nil)),
	}, nil
}

func (s *Store) Open(namespace, key string) (*os.File, error) {
	return os.Open(filepath.Join(s.root, namespace, filepath.FromSlash(key)))
}

func (s *Store) Delete(namespace, key string) error {
	if err := os.Remove(filepath.Join(s.root, namespace, filepath.FromSlash(key))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) DeleteNamespace(namespace string) error {
	if err := os.RemoveAll(filepath.Join(s.root, namespace)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
