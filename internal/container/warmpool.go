package container

import (
	"context"
	"sync"
	"time"
)

// DefaultMaxContainers is how many warm containers one function may have
// at once by default: enough for concurrent invocations to run side by
// side on a laptop without starting a container per request.
const DefaultMaxContainers = 4

type WarmContainer struct {
	ID       string
	Name     string
	Function string
	// Address is host:port where the container's emulator answers.
	Address   string
	LastUsed  time.Time
	CreatedAt time.Time
	// Slot numbers the function's containers from 0, so each gets a
	// stable, distinct name.
	Slot int

	busy bool
	// invokeMu serialises invocations: the emulator runs one at a time.
	invokeMu sync.Mutex
}

// WarmPool keeps up to max warm containers per function. An invocation
// takes an idle container, starts a new one while the function has fewer
// than max, or waits for one to come free, as Lambda scales execution
// environments with concurrency.
type WarmPool struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	pool map[string][]*WarmContainer
	// slots holds the slots in use per function, including containers
	// still starting.
	slots map[string]map[int]bool
	// freed is closed and replaced whenever a container of the function
	// comes free or a slot opens, waking invocations that wait.
	freed map[string]chan struct{}
	// starts and waits count cold starts and invocations that queued for
	// a container, so saturation is visible.
	starts, waits int
}

func NewWarmPool(ttl time.Duration) *WarmPool {
	return &WarmPool{
		ttl:   ttl,
		max:   DefaultMaxContainers,
		pool:  make(map[string][]*WarmContainer),
		slots: make(map[string]map[int]bool),
		freed: make(map[string]chan struct{}),
	}
}

// SetMax sets how many containers one function may have; below 1 means 1.
func (p *WarmPool) SetMax(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.max = max(1, n)
}

// PoolStats says how hard the pool is working.
type PoolStats struct {
	// Containers is how many warm containers exist across functions.
	Containers int `json:"containers"`
	// Busy is how many are running an invocation.
	Busy int `json:"busy"`
	// MaxPerFunction is the cap on one function's containers.
	MaxPerFunction int `json:"max_per_function"`
	// ColdStarts counts containers started.
	ColdStarts int `json:"cold_starts"`
	// Queued counts invocations that waited for a container because their
	// function was at its cap: Stratus, not AWS, was the bottleneck.
	Queued int `json:"queued"`
}

func (p *WarmPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := PoolStats{MaxPerFunction: p.max, ColdStarts: p.starts, Queued: p.waits}
	for _, list := range p.pool {
		for _, w := range list {
			s.Containers++
			if w.busy {
				s.Busy++
			}
		}
	}
	return s
}

// Acquire returns a container for key that only the caller uses until it
// calls Release (or Discard). create starts a container in the given slot.
func (p *WarmPool) Acquire(ctx context.Context, key string, create func(ctx context.Context, slot int) (*WarmContainer, error), healthy func(context.Context, *WarmContainer) (bool, error), cleanup func(context.Context, *WarmContainer) error) (*WarmContainer, error) {
	waited := false
	for {
		p.mu.Lock()
		if w := p.takeIdle(key); w != nil {
			p.mu.Unlock()
			if ok, err := healthy(ctx, w); err == nil && ok {
				p.mu.Lock()
				w.LastUsed = time.Now()
				p.mu.Unlock()
				return w, nil
			}
			_ = p.Discard(ctx, w, cleanup)
			continue
		}
		if len(p.slots[key]) < p.max {
			slot := p.reserveSlot(key)
			p.starts++
			p.mu.Unlock()
			w, err := create(ctx, slot)
			p.mu.Lock()
			if err != nil {
				delete(p.slots[key], slot)
				p.wake(key)
				p.mu.Unlock()
				return nil, err
			}
			w.Slot, w.busy, w.LastUsed = slot, true, time.Now()
			p.pool[key] = append(p.pool[key], w)
			p.mu.Unlock()
			return w, nil
		}
		if !waited {
			waited = true
			p.waits++
		}
		ch := p.waiter(key)
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Release hands a container back after an invocation.
func (p *WarmPool) Release(w *WarmContainer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.busy = false
	w.LastUsed = time.Now()
	p.wake(w.Function)
}

// Discard removes one container from the pool and cleans it up, as after
// a timeout or a failed invocation.
func (p *WarmPool) Discard(ctx context.Context, w *WarmContainer, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	p.forget(w)
	p.mu.Unlock()
	return cleanup(ctx, w)
}

// Remove discards every container of key.
func (p *WarmPool) Remove(ctx context.Context, key string, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	list := p.pool[key]
	for _, w := range list {
		p.forget(w)
	}
	p.mu.Unlock()
	var firstErr error
	for _, w := range list {
		if err := cleanup(ctx, w); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ExpireIdle discards idle containers unused for longer than the TTL.
func (p *WarmPool) ExpireIdle(ctx context.Context, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	var expired []*WarmContainer
	now := time.Now()
	for _, list := range p.pool {
		for _, w := range list {
			if !w.busy && now.Sub(w.LastUsed) > p.ttl {
				expired = append(expired, w)
			}
		}
	}
	for _, w := range expired {
		p.forget(w)
	}
	p.mu.Unlock()

	for _, w := range expired {
		if err := cleanup(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

func (p *WarmPool) CloseAll(ctx context.Context, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	var all []*WarmContainer
	for _, list := range p.pool {
		all = append(all, list...)
	}
	for _, w := range all {
		p.forget(w)
	}
	p.mu.Unlock()

	var firstErr error
	for _, w := range all {
		if err := cleanup(ctx, w); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// takeIdle marks and returns an idle container of key, dropping expired
// ones from consideration (ExpireIdle cleans them up).
func (p *WarmPool) takeIdle(key string) *WarmContainer {
	now := time.Now()
	for _, w := range p.pool[key] {
		if !w.busy && now.Sub(w.LastUsed) <= p.ttl {
			w.busy = true
			return w
		}
	}
	return nil
}

func (p *WarmPool) reserveSlot(key string) int {
	used := p.slots[key]
	if used == nil {
		used = map[int]bool{}
		p.slots[key] = used
	}
	slot := 0
	for used[slot] {
		slot++
	}
	used[slot] = true
	return slot
}

// forget drops w from the pool and frees its slot. Call with mu held.
func (p *WarmPool) forget(w *WarmContainer) {
	list := p.pool[w.Function]
	for i, x := range list {
		if x == w {
			p.pool[w.Function] = append(list[:i:i], list[i+1:]...)
			delete(p.slots[w.Function], w.Slot)
			break
		}
	}
	if len(p.pool[w.Function]) == 0 {
		delete(p.pool, w.Function)
	}
	p.wake(w.Function)
}

func (p *WarmPool) waiter(key string) chan struct{} {
	ch := p.freed[key]
	if ch == nil {
		ch = make(chan struct{})
		p.freed[key] = ch
	}
	return ch
}

func (p *WarmPool) wake(key string) {
	if ch := p.freed[key]; ch != nil {
		close(ch)
		delete(p.freed, key)
	}
}
