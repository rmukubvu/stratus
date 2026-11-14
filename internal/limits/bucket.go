package limits

import "time"

// tokenBucket admits up to rate requests per second on average, with
// bursts up to capacity. It refills continuously from the clock it is
// given, so tests drive it without sleeping. It is not safe for concurrent
// use; the governor serialises access.
type tokenBucket struct {
	rate     float64
	capacity float64
	tokens   float64
	last     time.Time
}

// newTokenBucket starts full, as AWS buckets do.
func newTokenBucket(rate, capacity float64, now time.Time) *tokenBucket {
	return &tokenBucket{rate: rate, capacity: capacity, tokens: capacity, last: now}
}

func (b *tokenBucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed*b.rate)
		b.last = now
	}
}

// has reports whether n tokens are available at now.
func (b *tokenBucket) has(now time.Time, n float64) bool {
	b.refill(now)
	return b.tokens >= n
}

// take removes n tokens. Call has first; take never refuses.
func (b *tokenBucket) take(n float64) { b.tokens -= n }

// semaphore counts work in flight against a cap.
type semaphore struct {
	max      int
	inflight int
}

func (s *semaphore) tryAcquire() bool {
	if s.inflight >= s.max {
		return false
	}
	s.inflight++
	return true
}

func (s *semaphore) release() {
	if s.inflight > 0 {
		s.inflight--
	}
}
