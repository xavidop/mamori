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
// Resolve, and mamori supplies the loop - so nobody hand-rolls a ticker. It is
// also the one place in the engine where a ref's retry cadence is expressible,
// which is why WithBackoff lands here and nowhere else: everything downstream
// (the forwarders, the reconciler) only consumes the Updates this loop decides
// to produce and has no way to ask a source to try again sooner or later. See
// WithBackoff's doc comment for what that means for natively-watched providers.
func pollWatch(ctx context.Context, p Provider, ref Ref, o *options) <-chan Update {
	ch := make(chan Update)
	interval := o.pollInterval
	bo := newBackoff(o)
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

		// Initial resolve so watchers see a baseline immediately. A failed
		// baseline is consecutive failure #1, not a free attempt: the first
		// retry after it is already spaced by the backoff base.
		if v, err := p.Resolve(ctx, ref); err != nil {
			if !errors.Is(err, ErrNotFound) {
				bo.fail()
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
			var d time.Duration
			if retry := bo.delay(); retry > 0 {
				// A failure streak is open, so the backoff governs the cadence
				// outright: it replaces the poll interval AND suppresses the
				// lease-refresh shortcut below. Letting the shortcut stay
				// active while failing would defeat the backoff entirely - it
				// only ever shortens, and the remaining lease life shrinks on
				// every pass, so it would converge into a tight retry loop
				// aimed at a backend that just proved it cannot serve one.
				// A lease we are unable to renew is not a reason to try harder.
				//
				// The backoff is jittered by the same fraction as the interval,
				// and for a stronger reason: a shared backend failing
				// synchronizes every client's failure instant, so an
				// un-jittered curve would have the whole fleet retry in
				// lockstep against a backend that is already unhealthy.
				d = jittered(retry, o.jitter)
			} else {
				d = jittered(interval, o.jitter)
				// Refresh before a known expiry, if that is sooner than the
				// interval. Aim for ~90% of the remaining lease life so we
				// renew slightly early.
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
					// Absence is an answer, not a failure: the backend was
					// reached and reported the ref missing, which is ordinary
					// default:/optional: territory. Backing off on it would
					// slow down discovering a ref that gets provisioned after
					// the process starts, so a not-found ends any open streak
					// rather than extending it.
					bo.reset()
					continue
				}
				bo.fail()
				if !emit(Value{}, err) {
					return
				}
				continue
			}
			bo.reset()
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
