package bbolt

import (
	"bytes"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Store struct {
	db *bolt.DB
}

func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt store %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Put(bucket, key string, val []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return fmt.Errorf("create bucket %q: %w", bucket, err)
		}
		return b.Put([]byte(key), val)
	})
}

func (s *Store) Get(bucket, key string) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}
		out = make([]byte, len(v))
		copy(out, v)
		return nil
	})
	return out, err
}

func (s *Store) Delete(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

func (s *Store) Scan(bucket, prefix string, fn func(k, v []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}

		c := b.Cursor()
		pfx := []byte(prefix)
		var k, v []byte
		if len(pfx) == 0 {
			k, v = c.First()
		} else {
			k, v = c.Seek(pfx)
		}

		for ; k != nil; k, v = c.Next() {
			if len(pfx) > 0 && !bytes.HasPrefix(k, pfx) {
				break
			}
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeletePrefix(bucket, prefix string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}

		c := b.Cursor()
		pfx := []byte(prefix)
		var keys [][]byte
		for k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
			key := make([]byte, len(k))
			copy(key, k)
			keys = append(keys, key)
		}
		for _, key := range keys {
			if err := b.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}
