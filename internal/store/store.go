package store

type Store interface {
	Close() error
	Put(bucket, key string, val []byte) error
	Get(bucket, key string) ([]byte, error)
	Delete(bucket, key string) error
	Scan(bucket, prefix string, fn func(k, v []byte) error) error
	DeletePrefix(bucket, prefix string) error
}
