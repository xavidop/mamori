package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// RateLimit throttles Resolve calls to at most rps per second (a simple, fair
// spacing limiter - each call waits until at least 1/rps has elapsed since the
// previous one). It protects rate-limited backends (Secrets Manager, Key Vault)
// from bursts. Watch is not rate-limited: it is a single long-lived subscription.
//
// If rps <= 0, no limiting is applied.
func RateLimit(rps float64, inner mamori.Provider, opts ...Option) mamori.Provider {
	cfg := newConfig(opts...)
	var mu sync.Mutex
	var next time.Time
	interval := time.Duration(0)
	if rps > 0 {
		interval = time.Duration(float64(time.Second) / rps)
	}

	resolve := func(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
		if interval > 0 {
			mu.Lock()
			now := cfg.clock.Now()
			wait := time.Duration(0)
			if now.Before(next) {
				wait = next.Sub(now)
				next = next.Add(interval)
			} else {
				next = now.Add(interval)
			}
			mu.Unlock()
			if wait > 0 {
				select {
				case <-cfg.clock.After(wait):
				case <-ctx.Done():
					return mamori.Value{}, ctx.Err()
				}
			}
		}
		return inner.Resolve(ctx, ref)
	}
	return newWrapper(inner, resolve, nil)
}
