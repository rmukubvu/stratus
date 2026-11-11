package store

import "testing"

type memStore map[string]map[string][]byte

func (m memStore) Close() error { return nil }
func (m memStore) Put(b, k string, v []byte) error {
	if m[b] == nil {
		m[b] = map[string][]byte{}
	}
	m[b][k] = v
	return nil
}
func (m memStore) Get(b, k string) ([]byte, error) { return m[b][k], nil }
func (m memStore) Delete(b, k string) error        { delete(m[b], k); return nil }
func (m memStore) DeletePrefix(b, p string) error  { delete(m, b); return nil }
func (m memStore) Scan(b, p string, fn func(k, v []byte) error) error {
	for k, v := range m[b] {
		if err := fn([]byte(k), v); err != nil {
			return err
		}
	}
	return nil
}

func TestScopedPrefixesBuckets(t *testing.T) {
	base := memStore{}
	view := Scoped(base, "@111122223333/eu-west-1/")
	if err := view.Put("sqs-queues", "orders", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := base["@111122223333/eu-west-1/sqs-queues"]["orders"]; !ok {
		t.Fatalf("expected prefixed bucket, got %v", base)
	}
	if got, _ := view.Get("sqs-queues", "orders"); string(got) != "x" {
		t.Fatalf("Get = %q", got)
	}
	if got, _ := base.Get("sqs-queues", "orders"); got != nil {
		t.Fatal("unprefixed bucket must stay untouched")
	}
}

func TestScopedWithoutPrefixIsBase(t *testing.T) {
	base := memStore{}
	if _, ok := Scoped(base, "").(memStore); !ok {
		t.Fatal("an empty prefix should return the base store")
	}
}
