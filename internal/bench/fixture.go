package bench

import (
	"bytes"
	"context"
	"fmt"
)

// Fixture is the seeded state every test expects to start from: a realistic
// mix of S3 objects, DynamoDB items and queued SQS messages.
type Fixture struct {
	Buckets          int
	ObjectsPerBucket int
	ObjectBytes      int
	Tables           int
	ItemsPerTable    int
	Queues           int
	MessagesPerQueue int
}

// DefaultFixture is a modest integration-test dataset.
func DefaultFixture() Fixture {
	return Fixture{
		Buckets: 4, ObjectsPerBucket: 100, ObjectBytes: 16 << 10,
		Tables: 3, ItemsPerTable: 500,
		Queues: 3, MessagesPerQueue: 50,
	}
}

func (f Fixture) bucket(i int) string { return fmt.Sprintf("bench-bucket-%d", i) }
func (f Fixture) table(i int) string  { return fmt.Sprintf("bench-table-%d", i) }
func (f Fixture) queue(i int) string  { return fmt.Sprintf("bench-queue-%d", i) }

// Seed creates the fixture through the AWS APIs, as a test suite's setup would.
func (f Fixture) Seed(ctx context.Context, aws AWS) error {
	body := bytes.Repeat([]byte("x"), f.ObjectBytes)
	for b := 0; b < f.Buckets; b++ {
		if err := aws.CreateBucket(ctx, f.bucket(b)); err != nil {
			return err
		}
		for o := 0; o < f.ObjectsPerBucket; o++ {
			if err := aws.PutObject(ctx, f.bucket(b), fmt.Sprintf("obj-%d", o), body); err != nil {
				return err
			}
		}
	}
	for t := 0; t < f.Tables; t++ {
		if err := aws.CreateTable(ctx, f.table(t)); err != nil {
			return err
		}
		if err := aws.PutItems(ctx, f.table(t), "seed", f.ItemsPerTable); err != nil {
			return err
		}
	}
	for q := 0; q < f.Queues; q++ {
		url, err := aws.CreateQueue(ctx, f.queue(q))
		if err != nil {
			return err
		}
		if err := aws.SendMessages(ctx, url, f.MessagesPerQueue); err != nil {
			return err
		}
	}
	return nil
}

// Mutate does what a typical test does to shared state: deletes an object,
// writes items, enqueues work and creates a resource of its own.
func (f Fixture) Mutate(ctx context.Context, aws AWS, test int) error {
	if f.Buckets > 0 && f.ObjectsPerBucket > 0 {
		if err := aws.DeleteObject(ctx, f.bucket(0), "obj-0"); err != nil {
			return err
		}
	}
	if f.Tables > 0 {
		if err := aws.PutItems(ctx, f.table(0), fmt.Sprintf("test-%d", test), 10); err != nil {
			return err
		}
	}
	if f.Queues > 0 {
		url, err := aws.QueueURL(ctx, f.queue(0))
		if err != nil {
			return err
		}
		if err := aws.SendMessages(ctx, url, 5); err != nil {
			return err
		}
	}
	return aws.CreateBucket(ctx, fmt.Sprintf("bench-test-%d", test))
}

// Verify fails unless the live state is exactly the seeded fixture, so a
// strategy only counts if it really isolates tests.
func (f Fixture) Verify(ctx context.Context, aws AWS) error {
	buckets, err := aws.CountBuckets(ctx)
	if err != nil {
		return err
	}
	if buckets != f.Buckets {
		return fmt.Errorf("%d buckets exist, want %d: a test's resources leaked", buckets, f.Buckets)
	}
	for b := 0; b < f.Buckets; b++ {
		n, err := aws.CountObjects(ctx, f.bucket(b))
		if err != nil {
			return err
		}
		if n != f.ObjectsPerBucket {
			return fmt.Errorf("%s has %d objects, want %d", f.bucket(b), n, f.ObjectsPerBucket)
		}
	}
	for t := 0; t < f.Tables; t++ {
		n, err := aws.CountItems(ctx, f.table(t))
		if err != nil {
			return err
		}
		if n != f.ItemsPerTable {
			return fmt.Errorf("%s has %d items, want %d", f.table(t), n, f.ItemsPerTable)
		}
	}
	for q := 0; q < f.Queues; q++ {
		url, err := aws.QueueURL(ctx, f.queue(q))
		if err != nil {
			return err
		}
		n, err := aws.CountMessages(ctx, url)
		if err != nil {
			return err
		}
		if n != f.MessagesPerQueue {
			return fmt.Errorf("%s has %d messages, want %d", f.queue(q), n, f.MessagesPerQueue)
		}
	}
	return nil
}
