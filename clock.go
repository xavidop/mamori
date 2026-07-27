package mamori

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock abstracts time so the reconciler can be tested deterministically. All
// time-dependent engine code uses a Clock rather than the time package directly.
// The default is the system clock; pass a different one with WithClock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) *Timer
	// NewTicker returns a ticker that fires every d.
	NewTicker(d time.Duration) *Ticker
	// After is shorthand for NewTimer(d).C.
	After(d time.Duration) <-chan time.Time
}

// Timer is a Clock-provided one-shot timer. Its channel C receives the time it
// fires. Stop prevents a not-yet-fired timer from firing.
type Timer struct {
	C    <-chan time.Time
	stop func() bool
}

// Stop halts the timer, returning true if it had not yet fired.
func (t *Timer) Stop() bool { return t.stop() }

// Ticker is a Clock-provided periodic ticker. Its channel C receives ticks.
type Ticker struct {
	C    <-chan time.Time
	stop func()
}

// Stop halts the ticker. It does not close C.
func (t *Ticker) Stop() { t.stop() }

// systemClock delegates to the standard time package.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) *Timer {
	t := time.NewTimer(d)
	return &Timer{C: t.C, stop: t.Stop}
}

func (systemClock) NewTicker(d time.Duration) *Ticker {
	t := time.NewTicker(d)
	return &Ticker{C: t.C, stop: t.Stop}
}

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// SystemClock returns the default, real-time Clock.
func SystemClock() Clock { return systemClock{} }

// --- Fake clock for tests --------------------------------------------------

// FakeClock is a manually-driven Clock for deterministic tests. Advance moves
// time forward, firing any timers/tickers whose deadline is reached.
//
// Advance only ever fires the timers that are already registered when it runs,
// so a test that advances the clock on behalf of another goroutine must first
// wait for that goroutine to arm its timer. BlockUntil is how to wait; see its
// doc comment for the race it closes.
type FakeClock struct {
	mu       sync.Mutex
	now      time.Time
	waiters  []*waiter
	tickers  []*fakeTicker
	blockers []*blocker
	nextID   int
}

type waiter struct {
	id       int
	deadline time.Time
	ch       chan time.Time
	fired    bool
}

// blocker is one outstanding BlockUntil call: ch is closed once at least want
// timers are pending. It carries no deadline of its own - the caller's context
// is what bounds the wait.
type blocker struct {
	want int
	ch   chan struct{}
}

type fakeTicker struct {
	id       int
	interval time.Duration
	next     time.Time
	ch       chan time.Time
	stopped  bool
}

// NewFakeClock returns a FakeClock started at the given time (or a fixed epoch
// if start is the zero value).
func NewFakeClock(start time.Time) *FakeClock {
	if start.IsZero() {
		start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &FakeClock{now: start}
}

// Now returns the fake current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer registers a one-shot timer.
func (c *FakeClock) NewTimer(d time.Duration) *Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	w := &waiter{id: c.nextID, deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		w.fired = true
	} else {
		c.waiters = append(c.waiters, w)
		c.notifyBlockersLocked()
	}
	id := w.id
	return &Timer{C: w.ch, stop: func() bool { return c.removeWaiter(id) }}
}

// After registers a timer and returns its channel.
func (c *FakeClock) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C }

// NewTicker registers a periodic ticker.
func (c *FakeClock) NewTicker(d time.Duration) *Ticker {
	if d <= 0 {
		panic("mamori: FakeClock ticker interval must be > 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	t := &fakeTicker{id: c.nextID, interval: d, next: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	id := t.id
	return &Ticker{C: t.ch, stop: func() { c.stopTicker(id) }}
}

// BlockUntil waits until at least n timers are pending on the clock - that is,
// until n calls to NewTimer or After (across any goroutine) have registered a
// deadline that has not yet fired and has not been stopped - and returns nil.
// It returns ctx.Err() if the context is done first, so a test that is wrong
// about what it is waiting for fails on its own deadline with a clear message
// instead of hanging until the package test timeout.
//
// It exists because Advance is not a synchronization point. Advance fires only
// the timers already registered at the moment it runs, and a test typically
// advances the clock on behalf of a goroutine that arms its own timers: the
// polling watch adapter, the reconciler's debounce. Nothing orders that
// goroutine's NewTimer call against the test's Advance call. Lose that race and
// the goroutine registers its timer just after Advance walked the waiter list,
// computing its deadline from the already-advanced now - so the deadline lands
// in the future the test believes it has already passed, the timer never fires,
// and the test waits for an event that can no longer happen. The failure is
// silent at the point it happens and only surfaces later, as a timeout in a
// receive that looks unrelated.
//
// Blocking on the registration first is what makes the ordering real:
//
//	clk.BlockUntil(ctx, 1) // the watched goroutine has armed its timer
//	clk.Advance(2 * time.Second)
//
// The alternative - sleeping long enough to usually win the race - trades a
// deterministic test for a slow flaky one, which is the whole thing FakeClock
// exists to avoid.
//
// Two properties are worth knowing before choosing n. Only timers count, not
// tickers, and a NewTimer(d) with d <= 0 fires immediately without ever
// becoming pending, so it never counts. And n is compared against the
// instantaneous pending set, not a running total: firing a timer (Advance) or
// stopping it (Timer.Stop) drops the count again, so n describes what should be
// armed right now, not how many have been armed since the clock was created.
func (c *FakeClock) BlockUntil(ctx context.Context, n int) error {
	c.mu.Lock()
	if len(c.waiters) >= n {
		c.mu.Unlock()
		return nil
	}
	b := &blocker{want: n, ch: make(chan struct{})}
	c.blockers = append(c.blockers, b)
	c.mu.Unlock()

	select {
	case <-b.ch:
		return nil
	case <-ctx.Done():
		// notifyBlockersLocked closes ch while holding the lock, so taking the
		// lock here orders this against any concurrent satisfaction: if the
		// count was reached before ctx expired, removeBlocker will not find b
		// and the recheck below sees the closed channel. Report success in that
		// case rather than a spurious error.
		c.removeBlocker(b)
		select {
		case <-b.ch:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// notifyBlockersLocked releases every BlockUntil call whose count has now been
// reached. It must be called with c.mu held, and only after c.waiters grows:
// the count never has to be rechecked when a timer fires or is stopped, since
// that can only lower it. Closing under the lock is what makes a racing
// BlockUntil's ctx.Done path able to tell "satisfied" from "timed out".
func (c *FakeClock) notifyBlockersLocked() {
	n := len(c.waiters)
	kept := c.blockers[:0]
	for _, b := range c.blockers {
		if n >= b.want {
			close(b.ch)
			continue
		}
		kept = append(kept, b)
	}
	c.blockers = kept
}

func (c *FakeClock) removeBlocker(target *blocker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, b := range c.blockers {
		if b == target {
			c.blockers = append(c.blockers[:i], c.blockers[i+1:]...)
			return
		}
	}
}

func (c *FakeClock) removeWaiter(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.waiters {
		if w.id == id {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (c *FakeClock) stopTicker(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tickers {
		if t.id == id {
			t.stopped = true
		}
	}
}

// Advance moves the fake clock forward by d, firing any timers and tickers whose
// deadlines fall within the interval, in chronological order.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)

	type event struct {
		at time.Time
		fn func()
	}
	var events []event

	// Collect timer events.
	remaining := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.deadline.After(target) {
			ww := w
			events = append(events, event{at: ww.deadline, fn: func() {
				if !ww.fired {
					ww.fired = true
					ww.ch <- ww.deadline
				}
			}})
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining

	// Collect ticker events (may fire multiple times across the interval).
	for _, t := range c.tickers {
		if t.stopped {
			continue
		}
		for !t.next.After(target) {
			at := t.next
			tt := t
			events = append(events, event{at: at, fn: func() {
				select {
				case tt.ch <- at:
				default: // drop tick if consumer is slow, like time.Ticker
				}
			}})
			t.next = t.next.Add(t.interval)
		}
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	c.now = target
	c.mu.Unlock()

	// Fire outside the lock so consumers can register new waiters.
	for _, e := range events {
		e.fn()
	}
}
