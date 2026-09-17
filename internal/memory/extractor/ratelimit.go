package extractor

import (
	"context"
	"sync"
	"time"
)

// RateLimiter is a token bucket over backend calls (spec §4.1 P0-S5). One
// token is consumed per *attempted* call, retries included, before the
// classifier runs, so it is a bound on paid calls per hour rather than on
// concurrency. It starts empty: a restart never grants a burst.
type RateLimiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	perSec float64
	last   time.Time
	now    func() time.Time
}

// NewRateLimiter refills callsPerHour tokens per hour up to burst. A nil
// *RateLimiter is valid and never waits, which keeps tests and the
// "unlimited" configuration simple.
func NewRateLimiter(callsPerHour, burst int) *RateLimiter {
	if callsPerHour <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = 1
	}
	return &RateLimiter{
		burst:  float64(burst),
		perSec: float64(callsPerHour) / 3600,
		now:    time.Now,
	}
}

func (r *RateLimiter) refill() {
	now := r.now()
	if !r.last.IsZero() {
		r.tokens += now.Sub(r.last).Seconds() * r.perSec
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
	}
	r.last = now
}

// take consumes a token if one is available, otherwise reports how long
// until the next one accrues.
func (r *RateLimiter) take() (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill()
	if r.tokens >= 1 {
		r.tokens--
		return true, 0
	}
	return false, time.Duration((1 - r.tokens) / r.perSec * float64(time.Second))
}

// Wait blocks until a token is available or ctx is done.
func (r *RateLimiter) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for {
		ok, wait := r.take()
		if ok {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
