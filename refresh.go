package mamori

import "context"

// Refresh forces an immediate re-resolve of every field, bypassing poll
// intervals and per-ref backoff, and blocks until the resulting snapshot has
// been applied or rejected.
//
// It returns nil when a snapshot was applied and when nothing changed, and the
// rejection reason when the candidate failed validation or a PreApply gate. A
// SIGHUP handler therefore learns whether the reload actually worked, which is
// the whole reason this blocks rather than queueing:
//
//	for range sighup {
//	    if err := w.Refresh(ctx); err != nil {
//	        log.Printf("reload rejected, still serving the previous config: %v", err)
//	    }
//	}
//
// It does NOT bypass PreApply. A forced refresh is still gated; that is the
// point of having a gate, and a refresh is what an operator reaches for exactly
// when a credential has just rotated - the moment a gate matters most.
//
// Refresh is delivered to the reconciler goroutine over the same control
// channel Pin, PinCurrent, and Unpin use, so it serializes with normal
// reconciliation rather than racing it, and it answers errWatcherClosed after
// Close for the same reason they do (see sendPin in pin.go).
//
// It returns ErrReentrantCall, having re-resolved nothing, when called from
// inside a PreApply hook: that hook runs ON the goroutine which would have to
// service this command, so the call would be waiting for itself. Taking a
// context does not make that survivable - a hook calling
// Refresh(context.Background()), the obvious thing to write, would block until
// Close - so this is refused up front exactly as Pin is, rather than left to a
// deadline the caller may not have set.
//
// ctx bounds the wait, not the work. Cancelling it returns ctx.Err() and stops
// this call from waiting; it does not recall a command already handed to the
// reconciler, which re-resolves and applies as usual. There is no half-applied
// snapshot either way.
//
// While the watcher is pinned, a refresh still re-resolves, still runs the
// gate, and still advances Live and history - it just does not move Get, which
// is what the pin is for. It returns nil in that case: the snapshot was applied
// as far as the pin allows, and Unpin will publish it. Refresh does not
// silently unpin.
//
// For a field resolved through a mamori:// ref, Refresh re-reads the config
// server's current value. It does not make the server re-resolve its own
// upstream: the server exists so that N consumers cost one upstream watch
// rather than N, and letting any client force an upstream fetch would invert
// exactly that property.
func (w *Watcher[T]) Refresh(ctx context.Context) error {
	return w.sendPinCtx(ctx, pinCmd{kind: refresh}).err
}
