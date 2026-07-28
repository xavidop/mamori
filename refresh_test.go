package mamori

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// refreshBudget is the hard ceiling every test in this file puts on a Refresh
// that must return. Refresh blocks by design, so the failure mode of every bug
// it can have - a branch of the handler that forgets to reply, a reentrant call
// with no receiver - is a call that never comes back. Without a ceiling those
// bugs would hang `go test` until the package timeout rather than failing it,
// which is the difference between a red build and a mystery. Ten seconds is
// enormous compared to the microseconds these calls take when they work.
const refreshBudget = 10 * time.Second

// TestRefreshAppliesChangeBetweenPolls sets the poll interval to an hour, and
// changes the provider's value WITHOUT pushing it to any watcher, so neither a
// poll tick nor a native watch notification can produce the new value: only
// Refresh can.
func TestRefreshAppliesChangeBetweenPolls(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rf://a"`
	}
	p := newWatchProvider("rf")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("a", "second", "v2") // silent: no push, so nothing is watching it

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second (Refresh must force a re-resolve)", got)
	}
}

// TestRefreshWithNothingChangedReturnsNil pins the other half of Refresh's
// contract: nil means "the configuration Get returns is current", which covers
// both "a new snapshot was applied" and "there was nothing to apply". A refresh
// that found no change must not manufacture a Change event either, or a SIGHUP
// loop would spam OnChange with reloads that changed nothing.
func TestRefreshWithNothingChangedReturnsNil(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rn://a"`
	}
	p := newWatchProvider("rn")
	p.set("a", "only", "v1")

	var changes atomic.Int64
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
		OnChange(func(Change[cfg]) { changes.Add(1) }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with nothing changed: %v", err)
	}
	if got := w.Get().A; got != "only" {
		t.Errorf("Get().A = %q, want only", got)
	}
	if n := changes.Load(); n != 0 {
		t.Errorf("OnChange fired %d times for a refresh that changed nothing, want 0", n)
	}
}

// TestRefreshRunsThePreApplyGate proves a forced refresh is still gated. The
// gate exists to answer "does this configuration actually work"; a refresh that
// skipped it would be the one path that publishes a snapshot nothing verified,
// and it would be the path an operator reaches for precisely when a credential
// has just rotated - the moment the gate matters most.
func TestRefreshRunsThePreApplyGate(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rg://a"`
	}
	p := newWatchProvider("rg")
	p.set("a", "first", "v1")

	seen := make(chan string, 4)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			seen <- ev.New.A
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// The initial load runs the gate too (decision D7); drain that invocation
	// so the assertion below is about the refresh and nothing else.
	select {
	case got := <-seen:
		if got != "first" {
			t.Fatalf("initial gate saw %q, want first", got)
		}
	case <-time.After(refreshBudget):
		t.Fatal("the PreApply gate did not run on the initial load")
	}

	p.set("a", "second", "v2")
	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	select {
	case got := <-seen:
		if got != "second" {
			t.Errorf("gate saw candidate %q, want second", got)
		}
	default:
		t.Fatal("Refresh applied a snapshot without running the PreApply gate")
	}
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second", got)
	}
}

// TestRefreshReturnsRejectionReason is the reason Refresh blocks rather than
// queueing: a SIGHUP handler has to learn whether the reload actually worked,
// and "the gate refused it" is the answer it most needs.
func TestRefreshReturnsRejectionReason(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rr://a"`
	}
	p := newWatchProvider("rr")
	p.set("a", "first", "v1")
	boom := errors.New("rejected by the gate")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "second" {
				return boom
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("a", "second", "v2")

	err = w.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh must return the rejection reason")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want one wrapping the hook's error", err)
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first", got)
	}
}

// TestRefreshReturnsValidationRejection covers the other refusal a candidate
// can meet: it never reaches the gate at all, because validation rejected it
// first. Refresh has to report that too, or a reload that produced an
// unusable configuration would look like a success.
func TestRefreshReturnsValidationRejection(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rv://a" validate:"oneof=first second"`
	}
	p := newWatchProvider("rv")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("a", "bogus", "v2")

	err = w.Refresh(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first (a rejected candidate must not reach Get)", got)
	}
}

func TestRefreshAfterCloseReturnsClosed(t *testing.T) {
	type cfg struct {
		A string `source:"rc://a"`
	}
	p := newWatchProvider("rc")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_ = w.Close()

	if err := w.Refresh(context.Background()); !errors.Is(err, errWatcherClosed) {
		t.Errorf("err = %v, want errWatcherClosed", err)
	}
}

func TestRefreshHonorsCallerContext(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rx://a"`
	}
	p := newWatchProvider("rx")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestRefreshFromInsidePreApplyHookFailsFast is the counterpart, for Refresh,
// of the reentrancy tests in preapply_reentrancy_test.go. The hazard is
// identical and so is the mechanism: the hook runs ON the reconciler goroutine,
// and Refresh is a command SERVICED by that goroutine, so a hook calling it
// would be waiting for a receiver that cannot exist until the hook it is
// running returns. Refresh does take a context, but that only makes the wedge
// escapable, not absent: a hook calling Refresh(context.Background()) - the
// obvious call to write - would still block until Close, with no
// reconciliation, no OnChange and no OnError in the meantime.
//
// The timeout is what makes this a test rather than a hang: without the
// detection this case does not fail, it stops.
func TestRefreshFromInsidePreApplyHookFailsFast(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rz://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rz")
	p.set("a", "first", "v1")

	// self is how the hook reaches the Watcher that is running it. It is an
	// atomic pointer because the hook also runs on the INITIAL load, on Watch's
	// own goroutine, before Watch has returned anything to store here.
	var self atomic.Pointer[Watcher[cfg]]
	refreshErr := make(chan error, 1)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			w := self.Load()
			if w == nil {
				return nil // initial load: no Watcher exists to reenter yet
			}
			refreshErr <- w.Refresh(context.Background())
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()
	self.Store(w)

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case err := <-refreshErr:
		if !errors.Is(err, ErrReentrantCall) {
			t.Fatalf("Refresh from inside a PreApply hook returned %v, want ErrReentrantCall", err)
		}
	case <-time.After(refreshBudget):
		t.Fatal("Refresh from inside a PreApply hook did not return: the watcher is wedged, which is the bug this detection exists to convert into an error")
	}

	// The refusal must be local to the offending call: the hook returned nil,
	// so the candidate is good and the flush that was already in flight has to
	// carry on and apply it.
	waitFlushed(t, w, 2)
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second: a refused reentrant Refresh must not disturb the flush that was already in flight", got)
	}
}

// TestRefreshKeepsAChainsLowerPositionState pins the one way a re-resolve can
// leave the engine knowing LESS than before it ran. A chain's re-resolve stops
// at the position that wins, so it learns nothing about the positions below -
// and if it overwrote their recorded state instead of leaving it alone, it
// would erase the fallback the engine had already established. Nothing would
// re-establish it: a watch source that has already delivered a value stays
// silent until that value next changes.
//
// The failure that would cause is not a stale read; it is a chain whose
// fallback silently stops working, discovered only when the primary next
// disappears - which is the exact moment the fallback exists for.
func TestRefreshKeepsAChainsLowerPositionState(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rm1://x,rm2://x"`
	}
	primary := newWatchProvider("rm1")
	fallback := newWatchProvider("rm2")
	fallback.set("x", "backup", "b1")

	// The primary is absent at startup, so Watch's own chain seeding records
	// BOTH positions: the primary as not-found, the fallback as the winner.
	// A zero debounce keeps the last step below a wait on the assertion itself
	// rather than on a coalescing window.
	w, err := Watch[cfg](context.Background(),
		WithProvider(primary), WithProvider(fallback), WithDebounce(0),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()
	if got := w.Get().A; got != "backup" {
		t.Fatalf("initial Get().A = %q, want backup (the primary is absent)", got)
	}

	// The primary appears, silently. Refresh is the only thing that can notice,
	// and noticing means its walk stops at the primary without ever consulting
	// the fallback.
	primary.set("x", "primary", "p1")
	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := w.Get().A; got != "primary" {
		t.Fatalf("Get().A = %q, want primary (a refresh must re-apply chain precedence)", got)
	}

	// The primary goes away again. The fallback has not moved and will never
	// re-announce itself, so the engine falls back only if the refresh left its
	// recorded state intact.
	primary.pushErr("x", ErrNotFound)
	waitUntil(t, 2*time.Second,
		"the chain to fall back to its lower position after the primary disappeared (a refresh must not erase what the engine already knew about that position)",
		func() bool { return w.Get().A == "backup" })
}

// TestRefreshWhilePinnedAdvancesLiveNotGet: a pin freezes what Get returns, and
// a forced refresh does not overrule that. It re-resolves, gates, and advances
// Live and history exactly as any other reconciliation does while pinned, so
// Unpin has the newest snapshot to publish - it just does not publish it early.
// Returning nil is the right answer: the refresh did everything the pin allows.
func TestRefreshWhilePinnedAdvancesLiveNotGet(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rq://a"`
	}
	p := newWatchProvider("rq")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p), WithPollInterval(time.Hour))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if v := w.PinCurrent(); v != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", v)
	}
	p.set("a", "second", "v2")
	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh while pinned: %v", err)
	}

	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q while pinned, want first: Refresh must not silently unpin", got)
	}
	if v, pinned := w.Pinned(); !pinned || v != 1 {
		t.Errorf("Pinned() = (%d, %v), want (1, true)", v, pinned)
	}
	if live := w.Status().Live; live != 2 {
		t.Errorf("Status().Live = %d after a refresh while pinned, want 2", live)
	}

	w.Unpin()
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q after Unpin, want second", got)
	}
}

// TestRefreshIsConcurrencySafe fires eight simultaneous Refresh calls at one
// watcher. Every one of them must come back: the control channel is unbuffered
// and serviced by a single goroutine, so a handler that replies zero times on
// some branch, or a caller that stops listening before the reply is sent,
// strands every call behind it.
func TestRefreshIsConcurrencySafe(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rk://a"`
	}
	p := newWatchProvider("rk")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// One WaitGroup, one close: closing the same channel from each of the eight
	// goroutines would panic on the second close, and would do it inside a
	// goroutine, taking the whole test binary down rather than failing this
	// test.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- w.Refresh(context.Background())
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(refreshBudget):
		t.Fatal("concurrent Refresh calls did not complete")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Refresh: %v", err)
		}
	}
}
