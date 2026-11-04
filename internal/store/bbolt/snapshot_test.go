package bbolt

import (
	"path/filepath"
	"reflect"
	"testing"
)

func openTestStore(t *testing.T, name string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustPut(t *testing.T, s *Store, bucket, key, val string) {
	t.Helper()
	if err := s.Put(bucket, key, []byte(val)); err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

func mustGet(t *testing.T, s *Store, bucket, key string) string {
	t.Helper()
	v, err := s.Get(bucket, key)
	if err != nil {
		t.Fatalf("get %s/%s: %v", bucket, key, err)
	}
	return string(v)
}

func TestBucketNamesReturnsSortedNames(t *testing.T) {
	s := openTestStore(t, "meta.db")
	mustPut(t, s, "sqs-queues", "a", "1")
	mustPut(t, s, "s3-buckets", "b", "2")

	names, err := s.BucketNames()
	if err != nil {
		t.Fatalf("bucket names: %v", err)
	}
	if want := []string{"s3-buckets", "sqs-queues"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestDeleteBucketsRemovesOnlyNamedBucketsAndIgnoresMissing(t *testing.T) {
	s := openTestStore(t, "meta.db")
	mustPut(t, s, "sqs-queues", "a", "1")
	mustPut(t, s, "s3-buckets", "b", "2")

	if err := s.DeleteBuckets("sqs-queues", "does-not-exist"); err != nil {
		t.Fatalf("delete buckets: %v", err)
	}

	names, _ := s.BucketNames()
	if want := []string{"s3-buckets"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestSnapshotRoundTripRestoresExactState(t *testing.T) {
	s := openTestStore(t, "meta.db")
	mustPut(t, s, "sqs-queues", "orders", "v1")
	mustPut(t, s, "s3-buckets", "assets", "v1")

	snapshotPath := filepath.Join(t.TempDir(), "snap.db")
	if err := s.WriteSnapshot(snapshotPath); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	// Mutate after the snapshot: change, add and remove state.
	mustPut(t, s, "sqs-queues", "orders", "v2")
	mustPut(t, s, "dynamodb-tables", "users", "new")
	if err := s.DeleteBuckets("s3-buckets"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := s.RestoreSnapshot(snapshotPath); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	names, _ := s.BucketNames()
	if want := []string{"s3-buckets", "sqs-queues"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("buckets after restore: got %v, want %v", names, want)
	}
	if got := mustGet(t, s, "sqs-queues", "orders"); got != "v1" {
		t.Fatalf("orders after restore: got %q, want v1", got)
	}
	if got := mustGet(t, s, "s3-buckets", "assets"); got != "v1" {
		t.Fatalf("assets after restore: got %q, want v1", got)
	}
}

func TestRestoreSnapshotFailsForMissingFileAndKeepsState(t *testing.T) {
	s := openTestStore(t, "meta.db")
	mustPut(t, s, "sqs-queues", "orders", "v1")

	if err := s.RestoreSnapshot(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("expected error for missing snapshot")
	}
	if got := mustGet(t, s, "sqs-queues", "orders"); got != "v1" {
		t.Fatalf("state changed after failed restore: %q", got)
	}
}
