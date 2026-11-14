package reqnote

import (
	"context"
	"fmt"
	"testing"
)

func TestNotesCollectKeysOnlyWhenAttached(t *testing.T) {
	AddKey(context.Background(), "orders", "a") // no notes: a no-op

	ctx, n := With(context.Background())
	AddKey(ctx, "orders", "a")
	AddKey(ctx, "orders", "b")
	if got := n.Keys(); len(got) != 2 || got[1] != (Key{"orders", "b"}) {
		t.Fatalf("keys %v", got)
	}
	for i := range 100 {
		AddKey(ctx, "orders", fmt.Sprint(i))
	}
	if len(n.Keys()) != maxKeys {
		t.Fatalf("kept %d keys, want %d", len(n.Keys()), maxKeys)
	}
	var none *Notes
	if none.Keys() != nil {
		t.Fatal("nil notes have keys")
	}
}
