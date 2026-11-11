package fsblob

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
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
	CRC32Base64  string
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
	crc32Hash := crc32.NewIEEE()
	sha256Hash := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(body, io.MultiWriter(md5Hash, crc32Hash, sha256Hash)))
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
		CRC32Base64:  base64.StdEncoding.EncodeToString(crc32Hash.Sum(nil)),
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

// DeleteNamespace removes a namespace, in this store and in the stores of
// other accounts and regions nested under it (see Scoped).
func (s *Store) DeleteNamespace(namespace string) error {
	dirs, err := filepath.Glob(filepath.Join(s.root, "@*", "*", namespace))
	if err != nil {
		return err
	}
	for _, dir := range append(dirs, filepath.Join(s.root, namespace)) {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Scoped returns a store for one account and region, nested in a
// directory named by prefix (see scope.Prefix). An empty prefix is s.
func (s *Store) Scoped(prefix string) *Store {
	if prefix == "" {
		return s
	}
	return &Store{root: filepath.Join(s.root, filepath.FromSlash(prefix))}
}
