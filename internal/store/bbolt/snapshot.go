package bbolt

import (
	"fmt"
	"os"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// BucketNames returns the names of all top-level buckets, sorted.
func (s *Store) BucketNames() ([]string, error) {
	var names []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			names = append(names, string(name))
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// DeleteBuckets removes the named buckets in a single transaction.
// Buckets that do not exist are ignored.
func (s *Store) DeleteBuckets(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range names {
			if err := tx.DeleteBucket([]byte(name)); err != nil && err != bolt.ErrBucketNotFound {
				return fmt.Errorf("delete bucket %q: %w", name, err)
			}
		}
		return nil
	})
}

// WriteSnapshot writes a transactionally consistent copy of the database to path.
func (s *Store) WriteSnapshot(path string) error {
	return s.db.View(func(tx *bolt.Tx) error {
		if err := tx.CopyFile(path, 0o600); err != nil {
			return fmt.Errorf("write snapshot %q: %w", path, err)
		}
		return nil
	})
}

// RestoreSnapshot replaces the entire contents of the live database with the
// contents of the snapshot at path. The swap happens in a single write
// transaction, so concurrent readers see either the old or the new state.
func (s *Store) RestoreSnapshot(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("open snapshot %q: %w", path, err)
	}
	src, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return fmt.Errorf("open snapshot %q: %w", path, err)
	}
	defer src.Close()

	return src.View(func(srcTx *bolt.Tx) error {
		return s.db.Update(func(dstTx *bolt.Tx) error {
			if err := dropAllBuckets(dstTx); err != nil {
				return err
			}
			return srcTx.ForEach(func(name []byte, srcBucket *bolt.Bucket) error {
				return copyBucket(dstTx, name, srcBucket)
			})
		})
	})
}

func dropAllBuckets(tx *bolt.Tx) error {
	var names [][]byte
	if err := tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		names = append(names, append([]byte(nil), name...))
		return nil
	}); err != nil {
		return err
	}
	for _, name := range names {
		if err := tx.DeleteBucket(name); err != nil {
			return fmt.Errorf("delete bucket %q: %w", name, err)
		}
	}
	return nil
}

type bucketCreator interface {
	CreateBucket(name []byte) (*bolt.Bucket, error)
}

func copyBucket(parent bucketCreator, name []byte, src *bolt.Bucket) error {
	dst, err := parent.CreateBucket(name)
	if err != nil {
		return fmt.Errorf("create bucket %q: %w", name, err)
	}
	return src.ForEach(func(k, v []byte) error {
		if v == nil {
			if nested := src.Bucket(k); nested != nil {
				return copyBucket(dst, k, nested)
			}
		}
		return dst.Put(k, v)
	})
}
