package mamori

import (
	"context"
	"fmt"
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
// The hook runs on the reconciler goroutine, because it has to complete before
// the swap and the OnChange dispatch queue is asynchronous and lossy by design
// (WithQueueDepth drops the oldest event when full); a gate cannot be delivered
// on a channel that is allowed to drop. Two consequences follow, and both
// matter:
//
// It is bounded by WithPreApplyTimeout, and the bound cannot be removed.
//
// It must not call back into the same Watcher. Get is lock-free and safe, but
// Refresh, Pin, PinCurrent, and Unpin are serviced by the very goroutine the
// hook is occupying, so calling one deadlocks until the timeout fires.
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
func WithPreApplyTimeout(d time.Duration) Option {
	return func(o *options) { o.preApplyTimeout = d }
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
func runPreApply[T any](parent context.Context, hook func(context.Context, Change[T]) error, timeout time.Duration, ev Change[T]) error {
	if hook == nil {
		return nil
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
