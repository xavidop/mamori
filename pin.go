package mamori

import "context"

// This file implements Pin, PinCurrent, and Unpin: a way to freeze Get at a
// known-good snapshot while sources keep being watched, then resume with one
// coalesced Change for everything that changed in the meantime.
//
// Pin/PinCurrent/Unpin are implemented as commands sent over a control
// channel to the reconciler goroutine (engine.handlePin in reconciler.go),
// not as an atomic flag flipped from the caller's goroutine. Unpin has to
// apply the live config to Get AND emit exactly one coalesced Change, and
// that has to happen on the reconciler goroutine to preserve serial OnChange
// delivery: doing it from a caller goroutine with an atomic flag would race
// the reconciler's own cfg.Store and enqueue calls for a concurrently
// landing update, and there is no way to make "flip a flag" and "the
// reconciler's next flush" agree on ordering without going through the same
// goroutine. Reads (Pinned, Status, History) stay lock-free: they only ever
// Load() a pointer the reconciler goroutine already finished publishing, the
// same pattern the rest of the package uses.
//
// Refresh (refresh.go) is a fourth command on this same channel, for the same
// reason: it re-resolves and flushes, which is exactly what the reconciler
// goroutine does, so it is delivered to that goroutine rather than duplicated
// beside it. The command kinds, the reply, the delivery and the reentrancy
// guard below are shared; only the handler differs.

// pinKind selects which control-channel command a pinCmd carries.
type pinKind int

const (
	pinAt      pinKind = iota // Pin(version): freeze at a specific retained snapshot
	pinCurrent                // PinCurrent(): freeze at whatever Get returns right now
	unpin                     // Unpin(): resume, applying the newest snapshot
	refresh                   // Refresh(): re-resolve every field now and apply the result
)

// pinCmd is sent over Watcher.control to the reconciler goroutine. reply is
// filled in by sendPin, never by the caller, so handlePin always has
// somewhere to answer.
type pinCmd struct {
	kind    pinKind
	version uint64
	reply   chan pinReply
}

// pinReply is handlePin's answer to a pinCmd: version carries the version
// pinned to (meaningful for pinCurrent, and echoed back for pinAt); err
// carries ErrNoSuchSnapshot for a pinAt version that is not retained, or
// errWatcherClosed if the watcher closed before the command could be
// delivered.
type pinReply struct {
	version uint64
	err     error
}

// sendPin delivers cmd to the reconciler goroutine and blocks for its reply.
// It never blocks forever, even when the reconciler goroutine has already
// exited or is mid-shutdown by the time this call is made, because it also
// selects on <-w.ctx.Done(): the reconciler goroutine's loop (see
// engine.loop) returns as soon as w.ctx is done, whether that is because
// Close cancelled it or because the PARENT context passed to Watch was
// cancelled independently of Close. w.ctx.Done() closes synchronously as
// part of the cancel call itself (a context.CancelFunc does not return
// until its Done channel is already closed), so a sendPin call blocked on
// control, or one made after the fact, unblocks here in both shutdown paths,
// not just the Close one, and returns errWatcherClosed: from the caller's
// perspective, "the reconciler is gone" reads the same regardless of which
// path caused it.
//
// There is one way to block on control forever that ctx.Done cannot rescue, and
// the guard below is it: a PreApply hook calling back into its own Watcher. The
// hook runs ON the reconciler goroutine (see preapply.go), so while it runs
// there is no receiver for control at all, and the caller waiting for one IS
// the goroutine that would have to become that receiver. Nothing short of Close
// ever resolves it, and until then the watcher does not reconcile, does not
// deliver OnChange and does not report an error - a silent, permanent wedge. So
// this refuses the command instead, and w.inPreApply is what makes the refusal
// precise: it holds the ID of the goroutine currently inside a hook, so only a
// caller that is genuinely waiting on itself is turned away. A pin command from
// any other goroutine, including one issued while a hook happens to be running,
// still queues on control and is serviced when the reconciler comes back to its
// select, exactly as it always was.
//
// The check is one atomic load on the path that matters: inPreApply is zero
// whenever no hook is running, which for a watcher with no PreApply gate is
// always, and the goroutine-ID lookup is reached only when a pin command truly
// overlaps a hook.
//
// Pin, PinCurrent and Unpin take no context, so this is the whole of their
// delivery. Refresh does take one, and goes through sendPinCtx below rather
// than around it: the guard above, the errWatcherClosed answer and the
// single-control-channel discipline are exactly what it needs too, and a second
// send path would have had to re-derive all three (and, on the evidence of the
// reentrancy bug this guard exists for, would have re-derived one of them
// wrong).
func (w *Watcher[T]) sendPin(cmd pinCmd) pinReply {
	// context.Background() is never cancelled, so both ctx.Done() branches
	// below are receives on a nil channel, which select can never choose. This
	// is therefore the same code path the three context-less commands have
	// always taken, not merely an equivalent one.
	return w.sendPinCtx(context.Background(), cmd)
}

// sendPinCtx is sendPin with a caller context that can abandon the wait. It is
// the shared body of both; see sendPin's doc comment for the reentrancy guard
// and for why w.ctx.Done() is what keeps delivery from blocking forever.
//
// ctx aborts the wait, and only the wait. Once the command is on the channel
// the reconciler goroutine is committed to running it (control is unbuffered,
// so the send completes only when handlePin has the command), and there is no
// way to recall it: a Refresh whose caller gave up still re-resolves and still
// applies whatever it finds. That is the honest behavior for a forced
// re-resolve - the alternative would be a half-applied snapshot - and it is why
// ctx.Err() is returned as "you stopped waiting", not as "nothing happened".
//
// The reply wait deliberately does NOT select on w.ctx.Done(), unlike the
// delivery above. Past the send, a reply is guaranteed: handlePin replies on
// every branch, to a buffered channel, so it cannot block and cannot be missed
// even if this caller is gone. A refresh's provider round trips can make that
// reply SLOW, which is exactly what ctx is here for; they cannot make it never
// come, which is the only thing a shutdown branch would be good for. What such
// a branch would add is a race in which a command that really did run reports
// errWatcherClosed because Close happened to land first - and for Refresh that
// is precisely the wrong answer, since a SIGHUP handler would then log a failed
// reload for a reload that worked.
func (w *Watcher[T]) sendPinCtx(ctx context.Context, cmd pinCmd) pinReply {
	if id := w.inPreApply.Load(); id != 0 && id == goroutineID() {
		return pinReply{err: ErrReentrantCall}
	}
	cmd.reply = make(chan pinReply, 1)
	select {
	case w.control <- cmd:
	case <-w.ctx.Done():
		return pinReply{err: errWatcherClosed}
	case <-ctx.Done():
		return pinReply{err: ctx.Err()}
	}
	select {
	case rep := <-cmd.reply:
		return rep
	case <-ctx.Done():
		return pinReply{err: ctx.Err()}
	}
}

// Pin freezes Get at the snapshot with the given version and stops applying
// reconciled updates to Get, though sources keep being watched and Status
// keeps reporting the diverging Live version underneath. It returns
// ErrNoSuchSnapshot if that version is not retained; raise WithHistory to
// pin further back.
//
// It returns ErrReentrantCall, immediately and without pinning anything, when
// called from inside a PreApply hook: the hook occupies the goroutine that
// would service this command. Call it from another goroutine.
func (w *Watcher[T]) Pin(version uint64) error {
	return w.sendPin(pinCmd{kind: pinAt, version: version}).err
}

// PinCurrent freezes Get at whatever snapshot it returns right now and
// returns that version. Unlike Pin, it always succeeds and needs no
// retained history, since it pins to the live snapshot rather than looking
// one up in it.
//
// It returns 0, pinning nothing, in the two cases where it cannot succeed: the
// watcher is closed, or it was called from inside a PreApply hook (see
// ErrReentrantCall - the signature has no room for one, and widening it would
// break every caller). Zero is unambiguous rather than a convenient lie:
// versions start at 1, so it can never collide with a version this really
// pinned, which is the same disambiguation Pinned relies on. Callers that need
// to distinguish the two causes, or to be sure at all, can check Pinned.
func (w *Watcher[T]) PinCurrent() uint64 {
	return w.sendPin(pinCmd{kind: pinCurrent}).version
}

// Unpin resumes applying reconciled updates to Get: it applies the newest
// validated snapshot and delivers exactly one Change whose Fields is the
// accumulated diff of everything that changed while pinned, no matter how
// many updates were reconciled (and recorded to history) in the meantime. It
// is a no-op if the watcher is not currently pinned.
//
// It is also a no-op when called from inside a PreApply hook (see
// ErrReentrantCall): it returns immediately and leaves the pin exactly as it
// found it, so the watcher stays pinned and Pinned still says so. It reports
// nothing, because it has nothing to report it with. Giving it an error return
// would be an incompatible change to a released API - it breaks every func()
// this method value is assigned to, t.Cleanup(w.Unpin) among them, and turns
// every existing call site into an unchecked-error lint finding - which is too
// much to charge every correct caller for a signal only an incorrect one can
// ever see. Pin, called the same wrong way, returns the error in full; the
// mistake is the same mistake, and diagnosing it once is enough.
func (w *Watcher[T]) Unpin() {
	w.sendPin(pinCmd{kind: unpin})
}

// Pinned reports whether Get is currently frozen at a pinned snapshot and,
// if so, which version. It reads the report the reconciler goroutine most
// recently published, the same lock-free pointer Status reads: while
// pinned, Report.Snapshot IS the pinned version being served (see
// engine.buildReport in report.go), so Pinned needs no extra field or
// control-channel round trip to answer, only Report.Pinned to disambiguate
// "pinned at version 0" (impossible; versions start at 1) from "not
// pinned".
func (w *Watcher[T]) Pinned() (uint64, bool) {
	rep := w.report.Load()
	if rep == nil || !rep.Pinned {
		return 0, false
	}
	return rep.Snapshot, true
}
