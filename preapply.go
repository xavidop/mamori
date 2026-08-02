package mamori

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
)

// defaultPreApplyTimeout bounds a PreApply hook when the caller sets none.
// Ten seconds is generous for the checks this hook exists for - opening a
// connection, exchanging a token - while still being short enough that a hook
// which hangs on an unresponsive backend does not stall reconciliation for
// long. See WithPreApplyTimeout for why the bound is mandatory.
const defaultPreApplyTimeout = 10 * time.Second

// PreApply installs a gate that runs before a reconciled snapshot becomes
// current. Returning a non-nil error rejects the candidate: Get keeps returning
// the last valid configuration, OnChange does not fire, and OnError receives a
// *PreApplyError describing the rejection.
//
// It exists for the checks struct validation cannot express, because they need
// I/O: that a rotated database password actually opens a connection, that a new
// API token is accepted by its issuer, that a reissued certificate chains to a
// trusted root. Validation answers "is this well-formed". PreApply answers
// "does this actually work", which is the question a rotation actually turns on.
//
//	w, err := mamori.Watch[Config](ctx,
//	    mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
//	        if !ev.Changed("DBPassword") {
//	            return nil
//	        }
//	        return pool.Ping(ctx, ev.New.DBPassword.Reveal())
//	    }),
//	)
//
// The hook runs on the reconciler goroutine: it must complete before the swap,
// and the OnChange dispatch queue is asynchronous and lossy by design, so a
// gate cannot ride on it. Two consequences follow.
//
// It is bounded by WithPreApplyTimeout, and the bound cannot be removed.
//
// It must not call back into the same Watcher. Get is safe: it Loads a pointer
// the reconciler already published, returning the snapshot this candidate would
// supersede, or the pinned one while pinned. Pin, PinCurrent, Unpin, and
// Refresh are not, since they are serviced by the very goroutine the hook
// occupies. Each is refused per call and the hook keeps running:
//
//   - Pin returns ErrReentrantCall, having pinned nothing.
//   - PinCurrent returns 0, which no real version ever is.
//   - Unpin does nothing and leaves the watcher pinned exactly as it was.
//   - Refresh returns ErrReentrantCall, having re-resolved nothing.
//
// Detection is keyed on which goroutine is inside the hook, not merely that one
// is, so a Pin from an unrelated goroutine that happens to overlap a hook waits
// its turn and is serviced normally.
//
// An OnError callback runs inline on the same goroutine and gets the same
// treatment. See ErrReentrantCall for the whole rule. OnChange is delivered
// from the dispatch queue on its own goroutine and is unaffected.
//
// It is typed to the same T passed to Watch, and runs on the initial load as
// well as on every subsequent update, so a credential that does not work is
// caught at startup rather than at the first rotation.
func PreApply[T any](fn func(ctx context.Context, ev Change[T]) error) Option {
	return func(o *options) { o.preApply = fn }
}

// WithPreApplyTimeout bounds how long a PreApply hook may run, defaulting to
// defaultPreApplyTimeout.
//
// The bound is mandatory rather than optional because the hook runs on the
// reconciler goroutine, which also services every other field's updates, the
// published Status report, and pin/unpin commands. An unbounded hook would
// wedge all of that.
//
// Exceeding the budget is a REJECTION, not an acceptance: on timeout mamori
// does not know whether the new configuration works, and applying it anyway
// would defeat the point of having a gate. A hook that always times out
// therefore stalls updates - loudly, emitting a *PreApplyError once per
// attempt - rather than quietly serving unverified configuration. That is the
// intended trade.
//
// A zero or negative d clamps to the default rather than being honored, the way
// WithHistory clamps a negative n. It cannot mean "no bound", because the bound
// is what keeps a hook from wedging the reconciler goroutine, and honoring it
// literally is worse still: context.WithTimeout(parent, 0) returns a context
// that is ALREADY expired, so every candidate would be refused on the deadline
// check - including the initial load, which would make Watch and Load fail at
// startup with a DeadlineExceeded that a caller writing WithPreApplyTimeout(0)
// is unlikely to read as "you disabled the gate". Elsewhere in this package a zero
// duration disables a feature (WithStale), so the one reading a caller is most
// likely to have in mind is the one meaning this option cannot express at all.
func WithPreApplyTimeout(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			d = defaultPreApplyTimeout
		}
		o.preApplyTimeout = d
	}
}

// PreApplyError is delivered to OnError when a PreApply hook rejects a
// candidate snapshot, and returned by Watch and Load when the hook rejects the
// initial configuration. Err is the hook's own error, or
// context.DeadlineExceeded when it exceeded WithPreApplyTimeout.
type PreApplyError struct {
	Err error
}

func (e *PreApplyError) Error() string {
	return fmt.Sprintf("mamori: pre-apply check rejected the configuration: %v", e.Err)
}

// Unwrap lets errors.Is and errors.As reach the hook's own error, including
// context.DeadlineExceeded for a hook that exceeded its budget.
func (e *PreApplyError) Unwrap() error { return e.Err }

// runPreApply invokes a typed hook under its timeout and wraps any refusal in
// a *PreApplyError. It returns nil when there is no hook, which is the common
// case, so callers need no nil check of their own.
//
// parent is the watcher's context, so cancelling the watcher also releases a
// hook blocked on a hanging backend rather than waiting out the full budget.
//
// mark is where the reentrancy detection is armed: for the duration of the
// hook it holds the ID of the goroutine running it, and 0 the rest of the time.
// sendPin (pin.go) compares it against its own caller's ID, which is what lets
// a pin command from inside the hook be refused while an identical command from
// any other goroutine keeps waiting its turn exactly as it always has. Callers
// with no watcher to protect pass nil: loadValue runs this on the caller's own
// goroutine, before Watch has constructed anything there is to reenter.
//
// The store is deferred, not merely written after the call, so a hook that
// panics cannot leave the mark set. See TestRunPreApplyClearsMarkWhenHookPanics
// for why that matters even though a panicking hook is not survivable today.
// Nothing is touched when there is no hook, so a watcher without a gate pays
// for none of this.
func runPreApply[T any](parent context.Context, hook func(context.Context, Change[T]) error, timeout time.Duration, ev Change[T], mark *atomic.Uint64) error {
	if hook == nil {
		return nil
	}
	if mark != nil {
		defer armReentrancy(mark)()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := hook(ctx, ev); err != nil {
		return &PreApplyError{Err: err}
	}
	// A hook that returned nil after its deadline passed did not actually
	// verify anything against a live deadline, so treat the expiry itself as
	// the refusal rather than trusting a result produced past the budget.
	if err := ctx.Err(); err != nil {
		return &PreApplyError{Err: err}
	}
	return nil
}

// armReentrancy marks mark with the calling goroutine's ID and returns the
// function that unmarks it, for use as `defer armReentrancy(mark)()` around a
// user callback this package runs INLINE on the reconciler goroutine. While the
// mark is set, sendPinCtx (pin.go) refuses a control-channel command issued from
// that same goroutine, which is the only caller that can never be answered.
//
// Three callbacks need this, and they are exactly the three that run there:
// runPreApply's hook, buildCandidate's derive loop, and emitErr's OnError
// (both in reconciler.go). OnChange does not, and must not have it - it runs
// on the dispatch goroutine, which receives from a queue rather than
// occupying the reconciler, so a Pin or Refresh from inside OnChange is an
// ordinary caller waiting its ordinary turn.
//
// It restores the PREVIOUS value rather than storing 0, which is what makes it
// safe to arm in more than one place. emitErr is reachable from inside flush's
// PreApply rejection path, so an inner frame that cleared the mark outright
// could unguard the remainder of an outer callback and quietly bring back the
// hang this detection exists to convert into an error. Restoring costs one extra
// word and removes the ordering dependency entirely; see TestArmReentrancyNests.
//
// Deferring the disarm rather than writing it after the call is what makes the
// mark's lifetime a property of THIS call rather than of the callback's
// cooperation: a callback that panics cannot leave the mark set, which would
// otherwise reject every later command issued from the reconciler goroutine.
func armReentrancy(mark *atomic.Uint64) func() {
	prev := mark.Swap(goroutineID())
	return func() { mark.Store(prev) }
}

// goroutineIDPrefix is the fixed opening of runtime.Stack's first line, which
// is documented to be "goroutine N [status]:" for the running goroutine.
const goroutineIDPrefix = "goroutine "

// goroutineID returns the calling goroutine's ID, or 0 if it cannot be
// determined.
//
// Go deliberately does not expose goroutine identity, so this reads it back out
// of the one place the runtime does print it: the header line of the
// goroutine's own stack trace. Identity is what the check needs: a mark that
// recorded only THAT a callback is running, rather than which goroutine, would
// refuse a Pin from any unrelated caller that merely overlapped one, with an
// error telling it to do the thing it was already doing.
//
// runtime.Stack(buf, false) walks only the calling goroutine, so it does not
// stop the world, and a small buffer truncates the OUTPUT rather than the walk,
// so cost stays proportional to stack depth - a couple of microseconds a few
// frames down, ~40us at depth 200. Arming (once per PreApply hook invocation
// and once per OnError delivery) runs near the bottom of the reconciler
// goroutine's own shallow stack, where that figure holds; checking, in
// sendPinCtx (pin.go), runs on the caller's own stack, which can be arbitrarily
// deep, but is reached only when a command actually overlaps a running
// callback - zero cost otherwise.
//
// Every failure path returns 0, the same value the mark holds when no callback
// is running, so a parse that stopped matching the runtime's format would
// silently disable the detection rather than manufacture a false match: a
// missed detection is a bug, a false one would break correct code.
func goroutineID() uint64 {
	var buf [40]byte
	n := runtime.Stack(buf[:], false)
	line := buf[:n]
	if len(line) <= len(goroutineIDPrefix) || string(line[:len(goroutineIDPrefix)]) != goroutineIDPrefix {
		return 0
	}
	var id uint64
	digits := 0
	for _, c := range line[len(goroutineIDPrefix):] {
		if c < '0' || c > '9' {
			break
		}
		// 19 digits is the widest value that cannot overflow uint64. A wider
		// run is not a goroutine ID, and wrapping one into a plausible-looking
		// number is the one outcome this must not produce.
		if digits == 19 {
			return 0
		}
		id = id*10 + uint64(c-'0')
		digits++
	}
	if digits == 0 {
		return 0
	}
	return id
}
