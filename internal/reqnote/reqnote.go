// Package reqnote lets a service note facts about the request it is
// handling, such as the DynamoDB table and partition key an item lives
// on, for whoever observes the request once it finishes. The HTTP layer
// attaches notes to a request's context only while someone is listening,
// so noting is free otherwise.
package reqnote

import (
	"context"
	"sync"
)

// Key is a resource a request touched and, for partitioned stores, the
// partition it touched.
type Key struct {
	Resource  string
	Partition string
}

// maxKeys bounds the keys kept per request; a 100-item batch notes the
// first ones, which is enough to see where traffic lands.
const maxKeys = 32

// Notes collects one request's notes. It is safe for concurrent use.
type Notes struct {
	mu   sync.Mutex
	keys []Key
}

type ctxKey struct{}

// With returns a context that collects notes, and the notes.
func With(ctx context.Context) (context.Context, *Notes) {
	n := &Notes{}
	return context.WithValue(ctx, ctxKey{}, n), n
}

// AddKey notes that the request touched resource, on partition. It does
// nothing when nobody collects notes.
func AddKey(ctx context.Context, resource, partition string) {
	n, _ := ctx.Value(ctxKey{}).(*Notes)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.keys) < maxKeys {
		n.keys = append(n.keys, Key{Resource: resource, Partition: partition})
	}
}

// Keys returns the keys noted so far.
func (n *Notes) Keys() []Key {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Key(nil), n.keys...)
}
