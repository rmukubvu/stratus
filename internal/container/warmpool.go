package container

import (
	"context"
	"sync"
	"time"
)

type WarmContainer struct {
	ID        string
	Name      string
	Function  string
	HostPort  int
	LastUsed  time.Time
	CreatedAt time.Time
}

type WarmPool struct {
	mu   sync.Mutex
	ttl  time.Duration
	pool map[string]*WarmContainer
}

func NewWarmPool(ttl time.Duration) *WarmPool {
	return &WarmPool{
		ttl:  ttl,
		pool: make(map[string]*WarmContainer),
	}
}

func (p *WarmPool) GetOrCreate(ctx context.Context, key string, create func(context.Context) (*WarmContainer, error), healthy func(context.Context, *WarmContainer) (bool, error), cleanup func(context.Context, *WarmContainer) error) (*WarmContainer, error) {
	p.mu.Lock()
	existing := p.pool[key]
	if existing != nil {
		if time.Since(existing.LastUsed) <= p.ttl {
			p.mu.Unlock()
			ok, err := healthy(ctx, existing)
			if err == nil && ok {
				p.mu.Lock()
				existing.LastUsed = time.Now()
				p.mu.Unlock()
				return existing, nil
			}
		} else {
			p.mu.Unlock()
		}
		_ = cleanup(ctx, existing)
		p.mu.Lock()
		delete(p.pool, key)
		p.mu.Unlock()
	} else {
		p.mu.Unlock()
	}

	warm, err := create(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.pool[key] = warm
	p.mu.Unlock()
	return warm, nil
}

func (p *WarmPool) Remove(ctx context.Context, key string, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	warm := p.pool[key]
	delete(p.pool, key)
	p.mu.Unlock()
	return cleanup(ctx, warm)
}

func (p *WarmPool) ExpireIdle(ctx context.Context, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	var expired []*WarmContainer
	now := time.Now()
	for key, warm := range p.pool {
		if now.Sub(warm.LastUsed) > p.ttl {
			expired = append(expired, warm)
			delete(p.pool, key)
		}
	}
	p.mu.Unlock()

	for _, warm := range expired {
		if err := cleanup(ctx, warm); err != nil {
			return err
		}
	}
	return nil
}

func (p *WarmPool) CloseAll(ctx context.Context, cleanup func(context.Context, *WarmContainer) error) error {
	p.mu.Lock()
	var all []*WarmContainer
	for key, warm := range p.pool {
		all = append(all, warm)
		delete(p.pool, key)
	}
	p.mu.Unlock()

	var firstErr error
	for _, warm := range all {
		if err := cleanup(ctx, warm); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
