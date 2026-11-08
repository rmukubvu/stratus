// Package chaostest provides a chaos.Gate for testing the services that
// make event deliveries.
package chaostest

import (
	"context"
	"sync"

	"github.com/stratus/internal/chaos"
)

// Gate records each delivery and applies a scripted outcome to it.
type Gate struct {
	mu         sync.Mutex
	deliveries []chaos.Delivery
	// Outcome decides what happens to the nth delivery (1-based): return
	// nil to deliver, or an error to fail it without delivering. A nil
	// Outcome delivers everything.
	Outcome func(n int, d chaos.Delivery) error
}

// Deliver implements chaos.Gate.
func (g *Gate) Deliver(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
	g.mu.Lock()
	g.deliveries = append(g.deliveries, d)
	n := len(g.deliveries)
	outcome := g.Outcome
	g.mu.Unlock()
	if outcome != nil {
		if err := outcome(n, d); err != nil {
			return err
		}
	}
	return deliver(ctx)
}

// Deliveries returns the deliveries seen so far.
func (g *Gate) Deliveries() []chaos.Delivery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]chaos.Delivery(nil), g.deliveries...)
}
