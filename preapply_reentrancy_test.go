package mamori

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// reentrancyBudget is the hard ceiling every test in this file puts on a call
// that must return promptly. It exists so a regression FAILS the suite instead
// of hanging it: the bug these tests pin is an unbounded block, and a test that
// simply waits for a reply that never comes would wedge `go test` itself until
// its package timeout, ten minutes later, with no useful message. Five seconds
// is enormous compared to the microseconds these calls take when they work.
const reentrancyBudget = 5 * time.Second

// TestPreApplyPinFromHookFailsFast pins the reentrancy hazard: a PreApply hook
// runs ON the reconciler goroutine, and Pin is a command SERVICED by that same
// goroutine over an unbuffered control channel. With the reconciler sitting
// inside the hook, nothing is receiving on w.control, and sendPin's only other
// select branch is w.ctx.Done() - which fires on Close and nothing else. Before
// detection existed this call blocked forever: no reconciliation, no OnChange,
// no OnError, no error anywhere, until something external called Close. The
// hook's own PreApply timeout does not rescue it, because the hook is parked
// inside sendPin, which never looks at the context the hook was handed.
func TestPreApplyPinFromHookFailsFast(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp1://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rp1")
	p.set("a", "first", "v1")

	// self is how the hook reaches the Watcher that is running it. It is an
	// atomic pointer rather than a plain variable because the hook also runs on
	// the INITIAL load (decision D7), on Watch's own goroutine, before Watch has
	// returned anything to store here - so the hook must tolerate reading nil,
	// and the later store must be safely visible to the reconciler goroutine.
	var self atomic.Pointer[Watcher[cfg]]
	pinErr := make(chan error, 1)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			w := self.Load()
			if w == nil {
				return nil // initial load: no Watcher exists to reenter yet
			}
			pinErr <- w.Pin(1)
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
	case err := <-pinErr:
		if !errors.Is(err, ErrReentrantCall) {
			t.Fatalf("Pin from inside a PreApply hook returned %v, want ErrReentrantCall", err)
		}
	case <-time.After(reentrancyBudget):
		t.Fatal("Pin from inside a PreApply hook did not return: the watcher is wedged, which is the bug this detection exists to convert into an error")
	}

	// The refusal must be local to the offending call. The hook returned nil, so
	// the candidate is good and the watcher has to carry on applying it.
	waitFlushed(t, w, 2)
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second: a refused reentrant call must not disturb the flush that was already in flight", got)
	}
}

// TestPreApplyPinCurrentFromHookFailsFast covers the same hazard through
// PinCurrent, which cannot report an error at all: its signature returns only
// the version it pinned to. Version 0 is the answer, and it is unambiguous
// rather than a lie, because versions start at 1 - the same "0 means it did not
// happen" disambiguation Pinned already relies on (see pin.go).
func TestPreApplyPinCurrentFromHookFailsFast(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp2://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rp2")
	p.set("a", "first", "v1")

	var self atomic.Pointer[Watcher[cfg]]
	got := make(chan uint64, 1)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			w := self.Load()
			if w == nil {
				return nil
			}
			got <- w.PinCurrent()
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
	case v := <-got:
		if v != 0 {
			t.Fatalf("PinCurrent from inside a PreApply hook returned version %d, want 0 (versions start at 1, so 0 is the unambiguous did-not-happen answer)", v)
		}
	case <-time.After(reentrancyBudget):
		t.Fatal("PinCurrent from inside a PreApply hook did not return: the watcher is wedged")
	}

	// A refused PinCurrent must not have pinned anything.
	if v, pinned := w.Pinned(); pinned {
		t.Errorf("Pinned() = (%d, true) after a refused PinCurrent, want not pinned", v)
	}
}

// TestPreApplyUnpinFromHookIsAnObservableNoOp covers Unpin, which returns
// nothing and therefore cannot carry the error. Changing its signature to
// return one is an incompatible API change (it breaks `t.Cleanup(w.Unpin)` and
// every func() the method value is assigned to, and it would make every
// existing `w.Unpin()` call site a new errcheck finding), so Unpin keeps its
// signature and instead does nothing at all: it returns immediately and leaves
// the pin exactly as it found it. That is not silent - it is observable through
// Pinned, which is what this test asserts - and it is strictly better than the
// previous behavior, which was to never return.
func TestPreApplyUnpinFromHookIsAnObservableNoOp(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp3://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rp3")
	p.set("a", "first", "v1")

	var self atomic.Pointer[Watcher[cfg]]
	returned := make(chan struct{}, 1)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			w := self.Load()
			if w == nil {
				return nil
			}
			w.Unpin()
			returned <- struct{}{}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()
	self.Store(w)

	// Pin from here, where it is legal, so the hook's Unpin has a real pin to
	// fail to release.
	if v := w.PinCurrent(); v != 1 {
		t.Fatalf("PinCurrent from the test goroutine returned %d, want 1", v)
	}

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case <-returned:
	case <-time.After(reentrancyBudget):
		t.Fatal("Unpin from inside a PreApply hook did not return: the watcher is wedged")
	}

	if v, pinned := w.Pinned(); !pinned || v != 1 {
		t.Errorf("Pinned() = (%d, %v) after a refused Unpin, want (1, true): a refused Unpin must leave the pin exactly as it was", v, pinned)
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first: a refused Unpin must not release the pin", got)
	}
}

// TestPreApplyGetInsideHookIsSafe pins the other half of the rule the error
// message states. Get is lock-free - it Loads an atomic pointer the reconciler
// goroutine published - so it never goes near the control channel and is
// exactly as safe inside a hook as anywhere else. The detection must not have
// swept it up.
func TestPreApplyGetInsideHookIsSafe(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp4://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rp4")
	p.set("a", "first", "v1")

	var self atomic.Pointer[Watcher[cfg]]
	seen := make(chan string, 1)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			w := self.Load()
			if w == nil {
				return nil
			}
			// The candidate is not current yet, so Get must still be showing
			// the snapshot this one supersedes. That is the whole point of a
			// gate, and it is what makes Get useful inside a hook.
			seen <- w.Get().A
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
	case got := <-seen:
		if got != "first" {
			t.Errorf("Get().A inside the hook = %q, want first (the candidate is not current until the gate passes)", got)
		}
	case <-time.After(reentrancyBudget):
		t.Fatal("Get inside a PreApply hook did not return: Get must stay lock-free and safe inside a hook")
	}
	waitFlushed(t, w, 2)
}

// TestPinFromAnotherGoroutineIsUnaffected is the false-positive test, and it is
// the reason the mark records WHICH goroutine is inside the hook rather than
// merely THAT one is. A caller on an unrelated goroutine that happens to call
// Pin while a hook is running is doing nothing wrong: its command sits on the
// control channel until the hook finishes, and the reconciler then services it
// exactly as it always has. A bare "a hook is running somewhere" flag would
// reject that caller - non-deterministically, once per rotation, for a window
// as long as the hook's whole budget - and would tell it to call from another
// goroutine, which is precisely what it already did.
func TestPinFromAnotherGoroutineIsUnaffected(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp5://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("rp5")
	p.set("a", "first", "v1")

	inHook := make(chan struct{})
	release := make(chan struct{})
	// live gates the blocking behavior to hook invocations that happen on the
	// RECONCILER goroutine. The gate also runs on the initial load, on Watch's
	// own goroutine (decision D7), and a hook that parked there would hang
	// inside Watch itself, before this test had anything to release it with.
	var live, blocked atomic.Bool

	// WithHistory keeps version 1 retained: this Pin is delivered only AFTER the
	// hook releases the reconciler and the flush it was gating completes, so by
	// the time it is serviced the live version is already 2 and the default
	// retention would have evicted the version being pinned. That would fail the
	// Pin for a reason that has nothing to do with what this test is about.
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk), WithHistory(8),
		PreApply(func(_ context.Context, _ Change[cfg]) error {
			if !live.Load() || !blocked.CompareAndSwap(false, true) {
				return nil
			}
			close(inHook)
			<-release
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()
	live.Store(true)

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	<-inHook // the reconciler goroutine is now parked inside the hook

	pinned := make(chan error, 1)
	go func() { pinned <- w.Pin(1) }()

	// While the hook holds the reconciler, this Pin must simply WAIT. Getting an
	// answer here at all would mean the detection fired on a caller that never
	// reentered anything. The window is deliberately generous: if the scheduler
	// has not run the goroutine above yet, this test still cannot fail falsely,
	// because the only thing asserted in this window is the ABSENCE of a wrong
	// answer.
	select {
	case err := <-pinned:
		t.Fatalf("Pin from an unrelated goroutine returned %v while a hook was running, want it to wait its turn", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-pinned:
		if errors.Is(err, ErrReentrantCall) {
			t.Fatalf("Pin from an unrelated goroutine was refused as reentrant: the detection must key on WHICH goroutine is inside the hook, not merely that one is")
		}
		if err != nil {
			t.Fatalf("Pin from an unrelated goroutine = %v, want nil once the hook released the reconciler", err)
		}
	case <-time.After(reentrancyBudget):
		t.Fatal("Pin from an unrelated goroutine never completed after the hook returned")
	}
	if v, ok := w.Pinned(); !ok || v != 1 {
		t.Errorf("Pinned() = (%d, %v), want (1, true)", v, ok)
	}
}

// TestPinWithNoHookInstalledIsUnaffected is the zero-cost, zero-behavior-change
// case: no PreApply hook at all, so the mark is never set and sendPin takes
// exactly the path it always took.
func TestPinWithNoHookInstalledIsUnaffected(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rp6://a"`
	}
	p := newWatchProvider("rp6")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if v := w.PinCurrent(); v != 1 {
		t.Errorf("PinCurrent() = %d, want 1", v)
	}
	if err := w.Pin(1); err != nil {
		t.Errorf("Pin(1) = %v, want nil", err)
	}
	if err := w.Pin(99); !errors.Is(err, ErrNoSuchSnapshot) {
		t.Errorf("Pin(99) = %v, want ErrNoSuchSnapshot: unrelated failures must still be reported as themselves", err)
	}
	w.Unpin()
	if v, pinned := w.Pinned(); pinned {
		t.Errorf("Pinned() = (%d, true) after Unpin, want not pinned", v)
	}
}

// TestRunPreApplyClearsMarkWhenHookPanics pins the defer.
//
// It is deliberately written against runPreApply rather than against a live
// Watcher, because a panicking hook is NOT survivable on the real path: nothing
// in this package recovers, so a hook that panics on the reconciler goroutine
// takes the whole process down - the test binary included. There is no "and
// then a later Pin still works" to observe on a process that no longer exists.
//
// The defer is still not optional. It is what makes the mark's lifetime a
// property of the call rather than of the hook's cooperation, so the invariant
// cannot silently rot if a recover is ever added above (at which point a mark
// left set would permanently reject that watcher's Pin/PinCurrent/Unpin from
// the reconciler goroutine, converting one bug into a quieter one). This test
// asserts that invariant at the only level where a panic is observable.
func TestRunPreApplyClearsMarkWhenHookPanics(t *testing.T) {
	type cfg struct{ A string }
	var mark atomic.Uint64

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("runPreApply swallowed the hook's panic; it must propagate")
			}
		}()
		_ = runPreApply(context.Background(),
			func(context.Context, Change[cfg]) error { panic("hook blew up") },
			time.Second, Change[cfg]{}, &mark)
	}()

	if got := mark.Load(); got != 0 {
		t.Errorf("mark = %d after a panicking hook, want 0: a mark left set would reject every later pin command from the reconciler goroutine", got)
	}
}

// TestRunPreApplySetsAndClearsMark pins the mark's normal lifetime: set to the
// running goroutine's own ID for the duration of the hook, cleared on return.
func TestRunPreApplySetsAndClearsMark(t *testing.T) {
	type cfg struct{ A string }
	var mark atomic.Uint64

	var inside uint64
	err := runPreApply(context.Background(),
		func(context.Context, Change[cfg]) error {
			inside = mark.Load()
			return nil
		},
		time.Second, Change[cfg]{}, &mark)
	if err != nil {
		t.Fatalf("runPreApply: %v", err)
	}
	if want := goroutineID(); inside != want || inside == 0 {
		t.Errorf("mark inside the hook = %d, want the running goroutine's ID %d", inside, want)
	}
	if got := mark.Load(); got != 0 {
		t.Errorf("mark = %d after the hook returned, want 0", got)
	}
}

// TestRunPreApplyWithNoHookLeavesMarkAlone is the zero-cost claim, asserted
// rather than asserted-in-a-comment: with no hook there is nothing to mark, and
// runPreApply must not even look up a goroutine ID.
func TestRunPreApplyWithNoHookLeavesMarkAlone(t *testing.T) {
	type cfg struct{ A string }
	var mark atomic.Uint64
	if err := runPreApply(context.Background(), nil, time.Second, Change[cfg]{}, &mark); err != nil {
		t.Fatalf("runPreApply with no hook = %v, want nil", err)
	}
	if got := mark.Load(); got != 0 {
		t.Errorf("mark = %d with no hook installed, want 0", got)
	}
}

// TestGoroutineIDIsStableAndDistinct pins the two properties the detection
// actually depends on: the ID is stable across calls on one goroutine (so a
// hook that calls Pin is recognized as the same goroutine that entered the
// hook), and different goroutines get different IDs (so an unrelated caller is
// never mistaken for the one inside the hook). A parse failure returns 0, which
// disables detection and restores the previous behavior - it can never invent a
// false match - so a nonzero result is also asserted here.
func TestGoroutineIDIsStableAndDistinct(t *testing.T) {
	mine := goroutineID()
	if mine == 0 {
		t.Fatal("goroutineID() = 0: no goroutine has ID 0, so the parse failed")
	}
	if again := goroutineID(); again != mine {
		t.Errorf("goroutineID() = %d then %d on the same goroutine, want a stable value", mine, again)
	}
	other := make(chan uint64, 1)
	go func() { other <- goroutineID() }()
	if got := <-other; got == mine {
		t.Errorf("goroutineID() = %d on a different goroutine, want a value distinct from %d", got, mine)
	}
}
