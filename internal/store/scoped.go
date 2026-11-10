package store

// Scoped returns a view of base whose buckets are all prefixed. It is how
// each account and region gets its own metadata inside one database.
func Scoped(base Store, prefix string) Store {
	if prefix == "" {
		return base
	}
	return scoped{base: base, prefix: prefix}
}

type scoped struct {
	base   Store
	prefix string
}

// Close is a no-op: the view does not own the database.
func (s scoped) Close() error { return nil }

func (s scoped) Put(bucket, key string, val []byte) error {
	return s.base.Put(s.prefix+bucket, key, val)
}

func (s scoped) Get(bucket, key string) ([]byte, error) {
	return s.base.Get(s.prefix+bucket, key)
}

func (s scoped) Delete(bucket, key string) error {
	return s.base.Delete(s.prefix+bucket, key)
}

func (s scoped) Scan(bucket, prefix string, fn func(k, v []byte) error) error {
	return s.base.Scan(s.prefix+bucket, prefix, fn)
}

func (s scoped) DeletePrefix(bucket, prefix string) error {
	return s.base.DeletePrefix(s.prefix+bucket, prefix)
}
