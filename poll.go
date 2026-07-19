package mamori

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// pollWatch adapts a non-watchable Provider into a watch by resolving on an
// interval and emitting an Update only when the value changes (by Version, or by
// bytes when Version is empty). It honors Value.NotAfter by scheduling an earlier
// refresh before expiry. The returned channel is closed when ctx is cancelled.
//
// This is the single, canonical polling adapter: provider authors implement only
// Resolve, and mamori supplies the loop - so nobody hand-rolls a ticker.
func pollWatch(ctx context.Context, p Provider, ref Ref, o *options) <-chan Update {
	ch := make(chan Update)
	interval := o.pollInterval
	go func() {
		defer close(ch)

		var last Value
		haveLast := false

		emit := func(v Value, err error) bool {
			select {
			case ch <- Update{Value: v, Err: err}:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Initial resolve so watchers see a baseline immediately.
		if v, err := p.Resolve(ctx, ref); err != nil {
			if !errors.Is(err, ErrNotFound) {
				if !emit(Value{}, err) {
					return
				}
			}
		} else {
			last, haveLast = v, true
			if !emit(v, nil) {
				return
			}
		}

		for {
			d := jittered(interval, o.jitter)
			// Refresh before a known expiry, if that is sooner than the interval.
			// Aim for ~90% of the remaining lease life so we renew slightly early.
			if haveLast && !last.NotAfter.IsZero() {
				untilExpiry := last.NotAfter.Sub(o.clock.Now())
				if untilExpiry > 0 {
					leaseRefresh := untilExpiry - untilExpiry/10
					if leaseRefresh <= 0 {
						leaseRefresh = untilExpiry
					}
					if leaseRefresh < d {
						d = leaseRefresh
					}
				}
			}
			timer := o.clock.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			v, err := p.Resolve(ctx, ref)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if !emit(Value{}, err) {
					return
				}
				continue
			}
			if haveLast && !last.changed(v) {
				continue
			}
			last, haveLast = v, true
			if !emit(v, nil) {
				return
			}
		}
	}()
	return ch
}

// jittered returns d randomized by ±frac (frac in 0..1).
func jittered(d time.Duration, frac float64) time.Duration {
	if frac <= 0 || d <= 0 {
		return d
	}
	delta := float64(d) * frac
	// rand is fine here: jitter needs no cryptographic strength.
	off := (rand.Float64()*2 - 1) * delta
	out := time.Duration(float64(d) + off)
	if out <= 0 {
		out = d
	}
	return out
}
