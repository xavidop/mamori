package mamori

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

// pinKind selects which control-channel command a pinCmd carries.
type pinKind int

const (
	pinAt      pinKind = iota // Pin(version): freeze at a specific retained snapshot
	pinCurrent                // PinCurrent(): freeze at whatever Get returns right now
	unpin                     // Unpin(): resume, applying the newest snapshot
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
func (w *Watcher[T]) sendPin(cmd pinCmd) pinReply {
	cmd.reply = make(chan pinReply, 1)
	select {
	case w.control <- cmd:
		return <-cmd.reply
	case <-w.ctx.Done():
		return pinReply{err: errWatcherClosed}
	}
}

// Pin freezes Get at the snapshot with the given version and stops applying
// reconciled updates to Get, though sources keep being watched and Status
// keeps reporting the diverging Live version underneath. It returns
// ErrNoSuchSnapshot if that version is not retained; raise WithHistory to
// pin further back.
func (w *Watcher[T]) Pin(version uint64) error {
	return w.sendPin(pinCmd{kind: pinAt, version: version}).err
}

// PinCurrent freezes Get at whatever snapshot it returns right now and
// returns that version. Unlike Pin, it always succeeds and needs no
// retained history, since it pins to the live snapshot rather than looking
// one up in it.
func (w *Watcher[T]) PinCurrent() uint64 {
	return w.sendPin(pinCmd{kind: pinCurrent}).version
}

// Unpin resumes applying reconciled updates to Get: it applies the newest
// validated snapshot and delivers exactly one Change whose Fields is the
// accumulated diff of everything that changed while pinned, no matter how
// many updates were reconciled (and recorded to history) in the meantime. It
// is a no-op if the watcher is not currently pinned.
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
