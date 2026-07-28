package mamori

import "time"

// backoffState tracks one ref's consecutive-failure streak so the polling
// adapter can space out its retries. It is owned by a single pollWatch
// goroutine and never shared, so it needs no locking.
//
// The window is normalized once, at construction, rather than in WithBackoff:
// every other Option in reconcile.go is a plain setter, and normalizing here
// means a directly-constructed *options (tests, WatchRef) gets the same
// clamping a caller going through WithBackoff does.
type backoffState struct {
	base     time.Duration
	ceiling  time.Duration
	failures int
}

// newBackoff reads the configured window off o and normalizes it.
//
// A base of zero or less disables backoff outright - that is the default, and
// the shape a caller gets by passing WithBackoff(0, 0). A ceiling below the
// base is raised to the base: the alternative reading, honoring a cap under
// the base, would make every retry FASTER than the base the caller explicitly
// asked for, which is the opposite of what capping means. That also gives
// WithBackoff(d, 0) the sane meaning "retry every d while failing" instead of
// unbounded exponential growth into hours.
func newBackoff(o *options) *backoffState {
	b := &backoffState{base: o.backoffBase, ceiling: o.backoffMax}
	if b.base <= 0 {
		return &backoffState{} // disabled
	}
	if b.ceiling < b.base {
		b.ceiling = b.base
	}
	return b
}

// fail records one more consecutive failure.
func (b *backoffState) fail() { b.failures++ }

// reset ends the streak. It is called on every successful round trip with the
// backend, which includes a not-found: absence is an answer, not a failure.
func (b *backoffState) reset() { b.failures = 0 }

// delay returns the retry delay for the current streak: base, doubled once per
// consecutive failure beyond the first, held at the ceiling from then on.
//
// It returns 0 to mean "no backoff applies, use the normal poll interval",
// which covers both a disabled window and a healthy ref. Callers must treat 0
// as absence rather than as a zero-length delay.
func (b *backoffState) delay() time.Duration {
	if b.base <= 0 || b.failures <= 0 {
		return 0
	}
	d := b.base
	// Bounded by log2(ceiling/base) iterations, not by b.failures: a ref that
	// has been failing for days must not cost a longer loop than one that just
	// started. Returning at ceiling/2 is also what keeps the doubling from
	// overflowing int64, since d <= ceiling/2 implies d*2 <= ceiling.
	for i := 1; i < b.failures; i++ {
		if d > b.ceiling/2 {
			return b.ceiling
		}
		d *= 2
	}
	if d > b.ceiling {
		d = b.ceiling
	}
	return d
}
