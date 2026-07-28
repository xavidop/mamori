package mamori

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// backoffRef is the ref every test in this file polls. Nothing about it
// matters except that it is stable across the fakes below.
var backoffRef = Ref{Scheme: "bo", Path: "x"}

// backoffOpts builds the options a cadence test needs: a fake clock, jitter
// off so every armed delay is exact, a poll interval far away from any backoff
// step so the two can never be confused for one another, and the backoff
// window under test.
func backoffOpts(clk *FakeClock, base, cap time.Duration) *options {
	o := defaultOptions()
	o.clock = clk
	o.jitter = 0
	o.pollInterval = 30 * time.Second
	o.backoffBase, o.backoffMax = base, cap
	return o
}

// expectErrThenDelay reads one error update off ch and returns the delay the
// poll loop then arms.
//
// The read has to come first. pollWatch's emit sends on an unbuffered channel
// and the loop only reaches its next NewTimer once that send completes, so a
// test that blocks on the timer before draining the update deadlocks against
// the goroutine it is waiting for.
func expectErrThenDelay(t *testing.T, clk *FakeClock, ch <-chan Update) time.Duration {
	t.Helper()
	select {
	case u, open := <-ch:
		if !open {
			t.Fatal("update channel closed, want an error update")
		}
		if u.Err == nil {
			t.Fatalf("update = %+v, want an error", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no error update from the polling adapter")
	}
	blockUntilTimers(t, clk, 1)
	return armedDelay(t, clk)
}

// drainClosed cancels a poll watch and drains its channel to closure, so
// goleak sees the adapter goroutine exit.
func drainClosed(cancel context.CancelFunc, ch <-chan Update) {
	cancel()
	for range ch { //nolint:revive // draining to closure is the point
	}
}

// TestWithBackoffOptionTakesEffect is the regression test for WithBackoff
// having been a dead option: it set options.backoffBase/backoffMax and nothing
// in the engine ever read them, so a caller who asked for a 1s first retry got
// the 30s poll interval instead.
//
// It is deliberately driven through the PUBLIC surface - WatchRef with
// WithBackoff, not a hand-built *options - because that is the thing that was
// broken. Against the dead option the first armed delay is 30s (the poll
// interval) and this fails on the very first assertion.
func TestWithBackoffOptionTakesEffect(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}

	ctx, cancel := context.WithCancel(context.Background())
	ch := WatchRef(ctx, p, backoffRef,
		WithClock(clk),
		WithJitter(0),
		WithPollInterval(30*time.Second),
		WithBackoff(time.Second, 4*time.Second),
	)

	if got := expectErrThenDelay(t, clk, ch); got != time.Second {
		t.Fatalf("first retry delay = %v, want %v (the configured backoff base, not the poll interval)", got, time.Second)
	}
	clk.Advance(time.Second)
	if got := expectErrThenDelay(t, clk, ch); got != 2*time.Second {
		t.Fatalf("second retry delay = %v, want %v", got, 2*time.Second)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffGrowsAndCaps pins the growth curve: base, then a doubling per
// consecutive failure, held at backoffMax from then on.
func TestPollBackoffGrowsAndCaps(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}
	o := backoffOpts(clk, time.Second, 8*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	// The first entry is the delay armed after the INITIAL resolve fails: a
	// failed baseline is consecutive failure #1, not a free attempt.
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second, // capped
		8 * time.Second, // stays capped
		8 * time.Second,
	}
	for i, w := range want {
		got := expectErrThenDelay(t, clk, ch)
		if got != w {
			t.Fatalf("retry delay after failure %d = %v, want %v", i+1, got, w)
		}
		clk.Advance(got)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffCapBelowBaseIsClampedUp covers the degenerate window a caller
// can write by accident, WithBackoff(30s, 5s): a cap under the base is raised
// to the base rather than silently making every retry 5s (which would be
// FASTER than the base the caller asked for, the opposite of what a cap means).
func TestPollBackoffCapBelowBaseIsClampedUp(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}
	o := backoffOpts(clk, 5*time.Second, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	for i := range 3 {
		got := expectErrThenDelay(t, clk, ch)
		if got != 5*time.Second {
			t.Fatalf("retry delay after failure %d = %v, want the base %v", i+1, got, 5*time.Second)
		}
		clk.Advance(got)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffResetsOnSuccess covers the other half of the contract: a
// success ends the streak, the cadence returns to the poll interval, and a
// later failure starts again from the base rather than resuming where the
// previous streak left off.
func TestPollBackoffResetsOnSuccess(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}
	o := backoffOpts(clk, time.Second, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	if got := expectErrThenDelay(t, clk, ch); got != time.Second {
		t.Fatalf("retry delay after failure 1 = %v, want %v", got, time.Second)
	}
	clk.Advance(time.Second)
	if got := expectErrThenDelay(t, clk, ch); got != 2*time.Second {
		t.Fatalf("retry delay after failure 2 = %v, want %v", got, 2*time.Second)
	}

	// The provider recovers.
	p.setVal(Value{Bytes: []byte("v1"), Version: "1"})
	p.setErr(nil)
	clk.Advance(2 * time.Second)
	drainOne(t, ch, "v1")
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != o.pollInterval {
		t.Fatalf("delay after a successful resolve = %v, want the poll interval %v", got, o.pollInterval)
	}

	// Failing again restarts the curve from the base.
	p.setErr(errors.New("boom again"))
	clk.Advance(o.pollInterval)
	if got := expectErrThenDelay(t, clk, ch); got != time.Second {
		t.Fatalf("retry delay after the streak reset = %v, want the base %v", got, time.Second)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffIgnoresNotFound pins that ErrNotFound is absence, not
// failure. The backend answered, so there is nothing to back off from, and
// backing off would delay discovering a ref that gets provisioned later - the
// "deploy the app, create the secret afterwards" workflow default:/optional:
// exist to support. Not-found neither starts a streak nor survives one.
func TestPollBackoffIgnoresNotFound(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: ErrNotFound}
	o := backoffOpts(clk, time.Second, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	// pollWatch swallows not-found without emitting, so there is no update to
	// drain: go straight to the timer the loop armed.
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != o.pollInterval {
		t.Fatalf("delay after a not-found baseline = %v, want the poll interval %v", got, o.pollInterval)
	}
	clk.Advance(o.pollInterval)
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != o.pollInterval {
		t.Fatalf("delay after a not-found poll = %v, want the poll interval %v", got, o.pollInterval)
	}

	// A real failure starts a streak...
	p.setErr(errors.New("boom"))
	clk.Advance(o.pollInterval)
	if got := expectErrThenDelay(t, clk, ch); got != time.Second {
		t.Fatalf("retry delay after a real failure = %v, want the base %v", got, time.Second)
	}
	// ...and a subsequent not-found ends it, because the backend answered.
	p.setErr(ErrNotFound)
	clk.Advance(time.Second)
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != o.pollInterval {
		t.Fatalf("delay after not-found cleared the streak = %v, want the poll interval %v", got, o.pollInterval)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffDisabledByDefault is the upgrade-safety test. defaultOptions
// leaves the backoff window at zero, so a caller who never passed WithBackoff
// keeps exactly the pre-feature cadence: a failing ref is retried on the poll
// interval, neither sooner nor later. Turning the previously-inert 1s/1m
// default on would have made every existing deployment retry a just-failed
// backend 30x faster for the first few seconds.
func TestPollBackoffDisabledByDefault(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}

	o := defaultOptions()
	if o.backoffBase != 0 || o.backoffMax != 0 {
		t.Fatalf("defaultOptions backoff = (%v, %v), want it off by default", o.backoffBase, o.backoffMax)
	}
	o.clock = clk
	o.jitter = 0
	o.pollInterval = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	for i := range 3 {
		got := expectErrThenDelay(t, clk, ch)
		if got != o.pollInterval {
			t.Fatalf("retry delay after failure %d = %v, want the unchanged poll interval %v", i+1, got, o.pollInterval)
		}
		clk.Advance(got)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffIsJittered covers design point 3. Backoff is randomized by
// the same WithJitter fraction as the poll interval, because a shared backend
// failing synchronizes every client's failure instant: unjittered exponential
// backoff would then have the whole fleet retry in lockstep at base, 2*base,
// 4*base, which is a synchronized retry storm aimed at a backend that is
// already unhealthy.
//
// The bounds assertion is what catches jitter being applied to the poll
// interval instead of to the backoff step (30s is nowhere near the +/-50% band
// around 1s). The distinctness assertion is what catches jitter not being
// applied at all; eight identical float64 draws from rand.Float64 is not a
// realistic flake (the collision probability is on the order of 2^-52 per
// pair), and no assertion here depends on wall-clock time.
func TestPollBackoffIsJittered(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bo", err: errors.New("boom")}
	o := backoffOpts(clk, time.Second, time.Second) // constant 1s step
	o.jitter = 0.5

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)

	seen := map[time.Duration]struct{}{}
	for i := range 8 {
		got := expectErrThenDelay(t, clk, ch)
		if got < 500*time.Millisecond || got > 1500*time.Millisecond {
			t.Fatalf("retry delay after failure %d = %v, outside the +/-50%% jitter band around the 1s backoff step (poll interval is %v)", i+1, got, o.pollInterval)
		}
		seen[got] = struct{}{}
		clk.Advance(got)
	}
	if len(seen) < 2 {
		t.Fatalf("every one of 8 backoff delays was identical (%v); jitter is not being applied to the backoff", seen)
	}
	drainClosed(cancel, ch)
}

// TestPollBackoffYieldsToLeaseRefreshOnlyWhenHealthy pins how backoff composes
// with pollWatch's early-lease-refresh shortcut. The shortcut shortens the next
// delay to ~90% of a Value.NotAfter lease's remaining life; left active during
// a failure streak it would fight the backoff and win, and since the remaining
// life shrinks on every pass it would converge into a tight retry loop at
// exactly the moment the backend is least able to serve it. So the shortcut
// applies only while the ref is healthy: once a streak is open the backoff
// governs outright.
func TestPollBackoffYieldsToLeaseRefreshOnlyWhenHealthy(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	start := clk.Now()
	p := &pollFake{scheme: "bo", val: Value{
		Bytes: []byte("lease1"), Version: "1", NotAfter: start.Add(10 * time.Second),
	}}
	o := backoffOpts(clk, time.Minute, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, backoffRef, o)
	drainOne(t, ch, "lease1")

	// Healthy: the lease shortcut still wins over the 30s poll interval.
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != 9*time.Second {
		t.Fatalf("healthy delay = %v, want the 9s lease refresh", got)
	}

	// Failing: the 60s backoff governs, even though the lease shortcut would
	// have asked for ~1s.
	p.setErr(errors.New("boom"))
	clk.Advance(9 * time.Second)
	if got := expectErrThenDelay(t, clk, ch); got != time.Minute {
		t.Fatalf("delay during a failure streak = %v, want the %v backoff, not the lease shortcut", got, time.Minute)
	}
	drainClosed(cancel, ch)
}

// TestBackoffDoesNotReachNativeWatchProviders covers design point 2. A
// WatchableProvider whose Watch starts successfully owns its own stream:
// watchRef hands that channel straight back and the polling adapter - the only
// place a retry delay is expressible - is never entered. WithBackoff therefore
// cannot influence it, and the assertion that makes that concrete is that no
// timer is ever armed on the clock, however many errors the native stream
// delivers.
func TestBackoffDoesNotReachNativeWatchProviders(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("bo")
	wp.set("x", "v1", "1")

	o := backoffOpts(clk, time.Second, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchRef(ctx, wp, backoffRef, o)

	for i := range 3 {
		wp.pushErr("x", errors.New("stream hiccup"))
		select {
		case u := <-ch:
			if u.Err == nil {
				t.Fatalf("update %d = %+v, want an error", i+1, u)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no error update %d from the native watch", i+1)
		}
		if n := armedTimers(clk); n != 0 {
			t.Fatalf("native watch armed %d timer(s) on the clock; backoff must not be in this path", n)
		}
	}
}

// failToStartWatcher is a WatchableProvider whose Watch never starts, the one
// case in which watchRef falls back to pollWatch for a provider that is
// nominally natively watchable.
type failToStartWatcher struct{ *pollFake }

func (failToStartWatcher) Watch(context.Context, Ref) (<-chan Update, error) {
	return nil, errors.New("native watch unavailable")
}

// TestBackoffAppliesToNativeWatchFallback is the boundary of design point 2:
// backoff does not reach a live native stream, but it does govern a
// WatchableProvider that failed to start one, because watchRef has fallen back
// to the polling adapter and the ref is being polled like any other.
func TestBackoffAppliesToNativeWatchFallback(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := failToStartWatcher{&pollFake{scheme: "bo", err: errors.New("boom")}}
	o := backoffOpts(clk, time.Second, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchRef(ctx, p, backoffRef, o)

	if got := expectErrThenDelay(t, clk, ch); got != time.Second {
		t.Fatalf("retry delay on the poll fallback = %v, want the backoff base %v", got, time.Second)
	}
	clk.Advance(time.Second)
	if got := expectErrThenDelay(t, clk, ch); got != 2*time.Second {
		t.Fatalf("second retry delay on the poll fallback = %v, want %v", got, 2*time.Second)
	}
	drainClosed(cancel, ch)
}

type staleBackoffConfig struct {
	V string `source:"bostale://x"`
}

// TestBackoffDelaysStaleErrorButNotStaleStatus covers design point 4, the one
// interaction worth being explicit about because it is a genuine (if opt-in)
// cost.
//
// mamori has two staleness surfaces and backoff hits exactly one of them:
//
//   - Status()/Health() recompute Age and Stale at READ time from LastOK
//     against the clock (report.go), so they are attempt-independent. A ref
//     that is backing off still goes Stale, and still flips Healthy to false,
//     at precisely maxAge. A readiness probe is unaffected.
//   - The *StaleError delivered to OnError is escalated inside
//     reportTerminalError (reconciler.go), which runs only when a failed
//     attempt produces an error update. There is no independent staleness
//     timer, so the escalation cannot happen sooner than the next attempt -
//     and backoff is what pushes that attempt out.
//
// So a long backoffMax delays the OnError signal by up to one backoff step
// while leaving the health surface exact. This test drives both halves at
// once: a 5m backoff step straddling a 1m stale threshold.
func TestBackoffDelaysStaleErrorButNotStaleStatus(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "bostale", val: Value{Bytes: []byte("v1"), Version: "1"}}

	const (
		staleAfter  = time.Minute
		backoffStep = 5 * time.Minute
		pollEvery   = 30 * time.Second
	)

	errs := make(chan error, 8)
	w, err := Watch[staleBackoffConfig](context.Background(),
		WithProvider(p),
		WithClock(clk),
		WithJitter(0),
		WithPollInterval(pollEvery),
		WithBackoff(backoffStep, backoffStep), // a flat, easy-to-straddle step
		WithStale(staleAfter),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Watch returns once its own initial Load has resolved, which says nothing
	// about pollWatch's baseline resolve having run yet - that happens on the
	// adapter's goroutine. Breaking the provider before the baseline lands
	// makes the BASELINE the first failure, which opens the streak an interval
	// earlier than this test is describing and shifts every deadline below.
	// Two calls (Load's, then the baseline's) is the handshake that the
	// adapter has taken its healthy reading; the timer assertion right after
	// confirms it is sitting on the ordinary poll interval, not a backoff.
	waitUntil(t, 2*time.Second, "pollWatch to take its baseline reading", func() bool {
		return p.calls.Load() >= 2
	})
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != pollEvery {
		t.Fatalf("delay while healthy = %v, want the poll interval %v", got, pollEvery)
	}

	// First failure: still inside the stale threshold, so a plain
	// *ProviderError, and the retry is pushed out by a full backoff step.
	p.setErr(errors.New("backend down"))
	clk.Advance(pollEvery)
	select {
	case e := <-errs:
		var pe *ProviderError
		if !errors.As(e, &pe) {
			t.Fatalf("first error = %T (%v), want *ProviderError", e, e)
		}
		var se *StaleError
		if errors.As(e, &se) {
			t.Fatalf("escalated to *StaleError before the stale threshold elapsed: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnError did not fire for the first failed poll")
	}
	blockUntilTimers(t, clk, 1)
	if got := armedDelay(t, clk); got != backoffStep {
		t.Fatalf("retry delay = %v, want the %v backoff step", got, backoffStep)
	}

	// Cross the stale threshold WITHOUT letting an attempt happen. The health
	// surface must reflect it immediately anyway.
	clk.Advance(staleAfter + time.Second)
	rep := w.Status()
	if len(rep.Fields) != 1 {
		t.Fatalf("Status fields = %d, want 1", len(rep.Fields))
	}
	if !rep.Fields[0].Stale {
		t.Fatalf("field not Stale past maxAge while backing off: %+v", rep.Fields[0])
	}
	if rep.Healthy {
		t.Fatalf("Healthy = true with a stale field: %+v", rep)
	}
	select {
	case e := <-errs:
		t.Fatalf("unexpected error delivered with no attempt in flight: %v", e)
	default:
	}

	// The attempt the backoff was holding back finally lands, and only now is
	// the staleness escalated to OnError.
	clk.Advance(backoffStep)
	select {
	case e := <-errs:
		var se *StaleError
		if !errors.As(e, &se) {
			t.Fatalf("error after maxAge = %T (%v), want *StaleError", e, e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnError did not escalate to *StaleError on the first attempt past maxAge")
	}
}

// TestBackoffDelayCurve exercises the delay computation directly, where the
// edge cases (a disabled window, a nonsensical cap, a cap that is not a
// power-of-two multiple of the base) are cheap to enumerate. want is indexed
// by consecutive-failure count, so want[0] is always the "no failure
// outstanding, use the poll interval" sentinel of 0.
func TestBackoffDelayCurve(t *testing.T) {
	const s = time.Second
	cases := []struct {
		name       string
		base, ceil time.Duration
		want       []time.Duration
	}{
		{
			name: "off when base is zero",
			want: []time.Duration{0, 0, 0, 0},
		},
		{
			name: "off when base is negative",
			base: -s, ceil: time.Minute,
			want: []time.Duration{0, 0, 0},
		},
		{
			name: "doubles then caps",
			base: s, ceil: 8 * s,
			want: []time.Duration{0, s, 2 * s, 4 * s, 8 * s, 8 * s, 8 * s},
		},
		{
			name: "zero cap clamps to the base",
			base: 2 * s,
			want: []time.Duration{0, 2 * s, 2 * s, 2 * s},
		},
		{
			name: "cap below base clamps to the base",
			base: 2 * s, ceil: s,
			want: []time.Duration{0, 2 * s, 2 * s, 2 * s},
		},
		{
			name: "cap that is not a power-of-two multiple still holds",
			base: s, ceil: 3 * s,
			want: []time.Duration{0, s, 2 * s, 3 * s, 3 * s},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackoff(&options{backoffBase: tc.base, backoffMax: tc.ceil})
			for failures, want := range tc.want {
				b.failures = failures
				if got := b.delay(); got != want {
					t.Fatalf("delay with %d consecutive failures = %v, want %v", failures, got, want)
				}
			}
		})
	}
}

// TestBackoffDelayDoesNotOverflow guards the doubling loop against both an
// int64 overflow and an unbounded iteration count: a ref that has been failing
// for days accumulates a large failure count, and the delay for it must still
// be computed in a bounded number of steps and still land on the cap.
func TestBackoffDelayDoesNotOverflow(t *testing.T) {
	b := newBackoff(&options{backoffBase: time.Nanosecond, backoffMax: time.Hour})
	for _, failures := range []int{64, 1_000, 1_000_000} {
		b.failures = failures
		if got := b.delay(); got != time.Hour {
			t.Fatalf("delay with %d consecutive failures = %v, want the %v cap", failures, got, time.Hour)
		}
	}
}
