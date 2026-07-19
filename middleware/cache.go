package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// Option configures cache/ratelimit middleware.
type Option func(*mwConfig)

type mwConfig struct {
	clock mamori.Clock
}

func newConfig(opts ...Option) mwConfig {
	c := mwConfig{clock: mamori.SystemClock()}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithClock overrides the clock used by time-based middleware (Cache, RateLimit),
// primarily for deterministic tests.
func WithClock(clk mamori.Clock) Option { return func(c *mwConfig) { c.clock = clk } }

type cacheEntry struct {
	val     mamori.Value
	expires time.Time
}

// Cache memoizes successful Resolve results for ttl, keyed by the ref. It reduces
// backend load and cost; watched values still update natively (Watch delegates to
// the inner provider). Errors and not-found results are not cached.
func Cache(ttl time.Duration, inner mamori.Provider, opts ...Option) mamori.Provider {
	cfg := newConfig(opts...)
	var mu sync.Mutex
	entries := map[string]cacheEntry{}

	resolve := func(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
		now := cfg.clock.Now()
		mu.Lock()
		if e, ok := entries[ref.Raw]; ok && now.Before(e.expires) {
			mu.Unlock()
			return e.val, nil
		}
		mu.Unlock()

		v, err := inner.Resolve(ctx, ref)
		if err != nil {
			return v, err
		}
		mu.Lock()
		entries[ref.Raw] = cacheEntry{val: v, expires: now.Add(ttl)}
		mu.Unlock()
		return v, nil
	}
	return newWrapper(inner, resolve, nil)
}
