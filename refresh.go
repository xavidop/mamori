package mamori

import "context"

// Refresh forces an immediate re-resolve of every field, bypassing poll
// intervals, and blocks until the resulting snapshot has been applied or
// rejected.
//
// It returns nil when a snapshot was applied and when nothing changed, and the
// rejection reason when the candidate failed a derive hook, validation, or a
// PreApply gate. A SIGHUP handler therefore learns whether the reload actually
// worked, which is the whole reason this blocks rather than queueing:
//
//	for range sighup {
//	    switch err := w.Refresh(ctx); {
//	    case err == nil:
//	        log.Println("reload applied")
//	    case ctx.Err() != nil:
//	        // The wait was abandoned, not the reload. Whether it applied is
//	        // unknown from here; Status reports what actually happened.
//	        log.Printf("stopped waiting for the reload: %v", err)
//	    default:
//	        log.Printf("reload rejected, still serving the previous config: %v", err)
//	    }
//	}
//
// Treating every non-nil error as a rejection is the mistake to avoid: a
// cancelled ctx returns ctx.Err() while the reload goes on to apply anyway.
//
// It does NOT bypass PreApply.
//
// Refresh is delivered to the reconciler goroutine over the same control
// channel Pin, PinCurrent, and Unpin use, so it serializes with normal
// reconciliation rather than racing it, and it answers errWatcherClosed after
// Close for the same reason they do (see sendPin in pin.go).
//
// It returns ErrReentrantCall, having re-resolved nothing, when called from
// inside a PreApply hook, a WithDerive hook, or an OnError callback: all three
// run ON the goroutine that would service this command, so the call would wait
// for itself. OnError is the one to watch for, since "the reload was rejected,
// retry it" is a natural thing to write there. OnChange is unaffected: it runs
// on the dispatch goroutine, so a Refresh from it is an ordinary call.
//
// ctx bounds the wait, not the work. Cancelling it returns ctx.Err() and stops
// this call from waiting; it does not recall a command already handed to the
// reconciler, which re-resolves and applies as usual. There is no half-applied
// snapshot either way.
//
// While pinned, a refresh still re-resolves, still runs the gate, and still
// advances Live and history. It just does not move Get, and returns nil:
// Unpin will publish the snapshot. Refresh does not silently unpin.
//
// For a field resolved through a mamori:// ref, Refresh re-reads the config
// server's current value; it does not make the server re-resolve its own
// upstream.
func (w *Watcher[T]) Refresh(ctx context.Context) error {
	return w.sendPinCtx(ctx, pinCmd{kind: refresh}).err
}
