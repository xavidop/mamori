package mamori_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// fakeMeter is a minimal mamori.Meter implementation for tests that need to
// observe which metrics calls the engine makes without pulling in the
// x/otel module (there is no in-core meter-capture test harness as of this
// writing). Counts are kept in atomics since the reconciler goroutine calls
// these methods concurrently with a test goroutine reading them.
type fakeMeter struct {
	refreshes atomic.Int64
}

func (m *fakeMeter) RecordResolve(scheme string, dur time.Duration, err error) {}
func (m *fakeMeter) RecordRefresh(scheme string)                               { m.refreshes.Add(1) }
func (m *fakeMeter) RecordWatchError(scheme string)                            {}
func (m *fakeMeter) RecordStale(scheme string)                                 {}
func (m *fakeMeter) RecordChangeDropped()                                      {}
func (m *fakeMeter) RecordApplyRejected(reason mamori.RejectReason)            {}

// waitForLive blocks until w.Status().Live reaches at least v. It is the
// Live counterpart to mamoritest.WaitForSnapshot's wait on Snapshot: Live is
// what keeps advancing while a watcher is pinned (Snapshot freezes at the
// pinned version by design), so these pin tests need a way to wait on it
// that WaitForSnapshot alone cannot provide.
func waitForLive[T any](t *testing.T, w *mamori.Watcher[T], v uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Status().Live >= v {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Live version %d not reached within 2s (current %d)", v, w.Status().Live)
}

// TestPinCurrentFreezesGetWhileLiveAdvances verifies the core split: after
// PinCurrent, Get and Status().Snapshot stay frozen at the pinned version
// while sources keep being reconciled underneath and Status().Live keeps
// advancing.
func TestPinCurrentFreezesGetWhileLiveAdvances(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-freeze")
	p.Set("cfg/level", "v1")

	type config struct {
		Level string `source:"mt-pin-freeze://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	pinned := w.PinCurrent()
	if pinned != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", pinned)
	}
	if ver, ok := w.Pinned(); !ok || ver != 1 {
		t.Fatalf("Pinned() = (%d, %v), want (1, true)", ver, ok)
	}

	p.Set("cfg/level", "v2")
	waitForLive(t, w, 2)

	if got := w.Get().Level; got != "v1" {
		t.Fatalf("Get().Level while pinned = %q, want v1 (frozen at the pin)", got)
	}
	rep := w.Status()
	if rep.Snapshot != 1 {
		t.Fatalf("Status().Snapshot = %d while pinned, want 1", rep.Snapshot)
	}
	if !rep.Pinned {
		t.Fatal("Status().Pinned = false while pinned, want true")
	}
	if rep.Live != 2 {
		t.Fatalf("Status().Live = %d, want 2 (reconciliation keeps advancing)", rep.Live)
	}
}

// TestUnpinCoalescesMultipleChangesIntoOneChange drives three separate
// applied flushes across two fields while pinned (including two on the same
// field, so a naive per-flush Change would produce three events), then
// confirms Unpin delivers exactly one Change whose Fields is the accumulated
// diff: this is the hardest property in the brief, that multiple Sets while
// pinned coalesce into ONE Change, not one per Set.
func TestUnpinCoalescesMultipleChangesIntoOneChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-unpin")
	p.Set("cfg/a", "a0")
	p.Set("cfg/b", "b0")

	type config struct {
		A string `source:"mt-pin-unpin://cfg/a"`
		B string `source:"mt-pin-unpin://cfg/b"`
	}

	events := make(chan mamori.Change[config], 4)
	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0),
		mamori.OnChange(func(ev mamori.Change[config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.PinCurrent(); got != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", got)
	}

	// Three flushes while pinned, driven one at a time (waiting for Live to
	// advance between each) so each lands as its own version rather than
	// getting swept into one debounce window: A changes twice, B once.
	p.Set("cfg/a", "a1")
	waitForLive(t, w, 2)
	p.Set("cfg/a", "a2")
	waitForLive(t, w, 3)
	p.Set("cfg/b", "b1")
	waitForLive(t, w, 4)

	// Nothing must have been delivered yet: Get and OnChange stay silent no
	// matter how many flushes land while pinned.
	select {
	case ev := <-events:
		t.Fatalf("unexpected Change delivered while still pinned: %+v", ev.Fields)
	default:
	}

	w.Unpin()

	var ev mamori.Change[config]
	select {
	case ev = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("Unpin did not deliver a Change")
	}

	// The hard assertion: exactly one Change, not three.
	select {
	case second := <-events:
		t.Fatalf("Unpin delivered a second Change: %+v", second.Fields)
	case <-time.After(200 * time.Millisecond):
	}

	if len(ev.Fields) != 2 {
		t.Fatalf("coalesced Change has %d fields, want 2 (A and B once each): %+v", len(ev.Fields), ev.Fields)
	}
	if !ev.Changed("A") || !ev.Changed("B") {
		t.Fatalf("coalesced Change.Fields = %+v, want A and B both present", ev.Fields)
	}
	if ev.Old.A != "a0" || ev.Old.B != "b0" {
		t.Fatalf("Change.Old = %+v, want the pinned snapshot {a0 b0}", ev.Old)
	}
	if ev.New.A != "a2" || ev.New.B != "b1" {
		t.Fatalf("Change.New = %+v, want the latest values {a2 b1}", ev.New)
	}

	if got := w.Get(); got.A != "a2" || got.B != "b1" {
		t.Fatalf("Get() after Unpin = %+v, want {a2 b1}", got)
	}
	rep := w.Status()
	if rep.Pinned {
		t.Fatal("Status().Pinned = true after Unpin, want false")
	}
	if rep.Snapshot != rep.Live {
		t.Fatalf("Status().Snapshot %d != Live %d after Unpin", rep.Snapshot, rep.Live)
	}
	if ver, ok := w.Pinned(); ok || ver != 0 {
		t.Fatalf("Pinned() after Unpin = (%d, %v), want (0, false)", ver, ok)
	}
}

// TestPinRetainedOlderVersionAndErrNoSuchSnapshot verifies Pin(version) can
// freeze Get at an older, still-retained snapshot (via WithHistory), that a
// failed Pin of an unretained version returns ErrNoSuchSnapshot, and that a
// failed Pin does not disturb whatever pin was already in effect.
func TestPinRetainedOlderVersionAndErrNoSuchSnapshot(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-history")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-history://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(3))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("cfg/level", "v1")
	mamoritest.WaitForSnapshot(t, w, 2)
	p.Set("cfg/level", "v2")
	mamoritest.WaitForSnapshot(t, w, 3)

	if err := w.Pin(2); err != nil {
		t.Fatalf("Pin(2): %v", err)
	}
	if got := w.Get().Level; got != "v1" {
		t.Fatalf("Get().Level after Pin(2) = %q, want v1 (version 2's config)", got)
	}
	if ver, ok := w.Pinned(); !ok || ver != 2 {
		t.Fatalf("Pinned() = (%d, %v), want (2, true)", ver, ok)
	}

	if err := w.Pin(999); !errors.Is(err, mamori.ErrNoSuchSnapshot) {
		t.Fatalf("Pin(999) error = %v, want ErrNoSuchSnapshot", err)
	}
	// A rejected Pin must leave the existing pin untouched.
	if got := w.Get().Level; got != "v1" {
		t.Fatalf("Get().Level after failed Pin(999) = %q, want v1 (unchanged)", got)
	}
	if ver, ok := w.Pinned(); !ok || ver != 2 {
		t.Fatalf("Pinned() after failed Pin(999) = (%d, %v), want (2, true) (unchanged)", ver, ok)
	}
}

// TestPinnedReflectsCurrentPinState exercises Pinned() across the not
// pinned -> pinned -> not pinned cycle, the property the brief's own example
// implementation deliberately got wrong.
func TestPinnedReflectsCurrentPinState(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-reports")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-reports://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if ver, ok := w.Pinned(); ok || ver != 0 {
		t.Fatalf("Pinned() before any Pin = (%d, %v), want (0, false)", ver, ok)
	}

	got := w.PinCurrent()
	if ver, ok := w.Pinned(); !ok || ver != got {
		t.Fatalf("Pinned() after PinCurrent = (%d, %v), want (%d, true)", ver, ok, got)
	}

	w.Unpin()
	if ver, ok := w.Pinned(); ok || ver != 0 {
		t.Fatalf("Pinned() after Unpin = (%d, %v), want (0, false)", ver, ok)
	}
}

// TestValidationFailureWhilePinnedReachesOnError is the other hard property:
// a candidate that fails validation while pinned must still reach OnError
// (the same as if the watcher were not pinned), while Get stays at the
// pinned value and Live does not advance for the rejected candidate, since
// buildCandidate rejects it before flush ever increments e.version.
func TestValidationFailureWhilePinnedReachesOnError(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-validate")
	p.Set("cfg/level", "info")

	type config struct {
		Level string `source:"mt-pin-validate://cfg/level" validate:"oneof=debug info warn error"`
	}

	onErr, errs := mamoritest.CaptureErrors()
	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), onErr)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.PinCurrent(); got != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", got)
	}

	p.Set("cfg/level", "BOGUS") // fails the oneof validation

	// ValidationError wraps the validator's own error, not one of mamori's
	// classification sentinels, so ErrorKind reports KindUnknown for it; the
	// errors.As check below confirms it really is the *ValidationError this
	// test is targeting, not some other KindUnknown error.
	got := mamoritest.WaitForError(t, errs, mamori.KindUnknown)
	var ve *mamori.ValidationError
	if !errors.As(got, &ve) {
		t.Fatalf("error = %T, want *mamori.ValidationError", got)
	}

	if l := w.Get().Level; l != "info" {
		t.Fatalf("Get().Level after a rejected candidate while pinned = %q, want info (pinned value)", l)
	}
	rep := w.Status()
	if rep.Live != 1 {
		t.Fatalf("Status().Live = %d after a rejected candidate, want 1 (must not advance)", rep.Live)
	}
	if rep.Snapshot != 1 || !rep.Pinned {
		t.Fatalf("Status() = %+v, want Snapshot=1 Pinned=true", rep)
	}
}

// TestPinCurrentRePinWhileAlreadyPinnedStaysAtServedVersion is the regression
// test for the Bug 1 fix: PinCurrent means "freeze at whatever Get returns
// right now," which is the currently-SERVED snapshot, not the live one. Before
// the fix, calling PinCurrent again while already pinned at an older version
// jumped pinnedVersion/pinnedConfig to the live snapshot without a matching
// cfg.Store, so Get stayed stale while Status/Pinned reported the new version
// -- Get() and Status() desynced. After the fix, re-pinning current while
// already pinned must be a no-op: Get, Status().Snapshot, and Pinned() all
// still agree on the originally-pinned version.
func TestPinCurrentRePinWhileAlreadyPinnedStaysAtServedVersion(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-repin-current")
	p.Set("cfg/level", "v1")

	type config struct {
		Level string `source:"mt-pin-repin-current://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.PinCurrent(); got != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", got)
	}

	// Live advances underneath the pin; Get must not follow it.
	p.Set("cfg/level", "v2")
	waitForLive(t, w, 2)

	// Re-pin current while already pinned: must be a no-op, not a jump to
	// the live version, since Get never moved.
	got := w.PinCurrent()
	if got != 1 {
		t.Fatalf("second PinCurrent() = %d, want 1 (re-pinning current while already pinned is a no-op)", got)
	}

	if l := w.Get().Level; l != "v1" {
		t.Fatalf("Get().Level after re-pin = %q, want v1 (unchanged, Get never moved)", l)
	}
	rep := w.Status()
	if rep.Snapshot != 1 {
		t.Fatalf("Status().Snapshot after re-pin = %d, want 1", rep.Snapshot)
	}
	if !rep.Pinned {
		t.Fatal("Status().Pinned = false after re-pin, want true")
	}
	if ver, ok := w.Pinned(); !ok || ver != 1 {
		t.Fatalf("Pinned() after re-pin = (%d, %v), want (1, true)", ver, ok)
	}

	// The core assertion: Get(), Status().Snapshot, and Pinned() must all
	// agree, both with each other and with what PinCurrent returned.
	pinnedVer, _ := w.Pinned()
	if rep.Snapshot != pinnedVer {
		t.Fatalf("Status().Snapshot %d != Pinned() version %d", rep.Snapshot, pinnedVer)
	}
	if rep.Snapshot != got {
		t.Fatalf("Status().Snapshot %d != second PinCurrent() return %d", rep.Snapshot, got)
	}
}

// TestPinRePinToDifferentRetainedVersionUpdatesGet complements the PinCurrent
// no-op case above: unlike PinCurrent, Pin(version) always moves Get to the
// requested version's config, even when re-pinning while already pinned. This
// confirms the pinAt branch (which already did cfg.Store correctly) keeps
// working after the Bug 1 fix to the pinCurrent branch.
func TestPinRePinToDifferentRetainedVersionUpdatesGet(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-repin-at")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-repin-at://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(3))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("cfg/level", "v1")
	mamoritest.WaitForSnapshot(t, w, 2)
	p.Set("cfg/level", "v2")
	mamoritest.WaitForSnapshot(t, w, 3)

	if err := w.Pin(1); err != nil {
		t.Fatalf("Pin(1): %v", err)
	}
	if got := w.Get().Level; got != "v0" {
		t.Fatalf("Get().Level after Pin(1) = %q, want v0", got)
	}

	// Re-pin to a different retained version while already pinned: Get must
	// move to that version's config.
	if err := w.Pin(2); err != nil {
		t.Fatalf("Pin(2): %v", err)
	}
	if got := w.Get().Level; got != "v1" {
		t.Fatalf("Get().Level after Pin(2) = %q, want v1 (moved to the newly pinned version)", got)
	}
	if ver, ok := w.Pinned(); !ok || ver != 2 {
		t.Fatalf("Pinned() after Pin(2) = (%d, %v), want (2, true)", ver, ok)
	}
	rep := w.Status()
	if rep.Snapshot != 2 {
		t.Fatalf("Status().Snapshot after Pin(2) = %d, want 2", rep.Snapshot)
	}
}

// TestUnpinOlderPinDiffIncludesFieldsChangedBeforeThePin is the regression
// test for the Bug 2 fix. It pins an OLDER retained version where a field
// (B) had already advanced past the pinned snapshot's value BEFORE Pin was
// even called -- not because it changed while pinned, but because it changed
// earlier, between two flushes that both landed before the pin. The
// coalesced diff Unpin emits must still name that field.
//
// Before the fix, pinnedApplied was seeded from the engine's live applied
// map at Pin-call time (which, for Pin(olderVersion), is already past the
// pinned snapshot for any field that changed between the pinned version and
// the live version). Since B does not change again after Pin(2), diffing
// against that bugged baseline finds B unchanged, wrongly omitting it from
// Change.Fields even though Change.Old.B != Change.New.B: Changed("B") would
// wrongly report false. This test fails against the pre-fix baseline (it
// reports only 1 field instead of 2) and passes once pinnedApplied for
// Pin(version) is seeded from the pinned snapshot's own applied map.
func TestUnpinOlderPinDiffIncludesFieldsChangedBeforeThePin(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-older-baseline")
	p.Set("cfg/a", "a0")
	p.Set("cfg/b", "b0")

	type config struct {
		A string `source:"mt-pin-older-baseline://cfg/a"`
		B string `source:"mt-pin-older-baseline://cfg/b"`
	}

	events := make(chan mamori.Change[config], 4)
	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(3),
		mamori.OnChange(func(ev mamori.Change[config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// version 2: A changes to a1; B is still b0 at this point.
	p.Set("cfg/a", "a1")
	mamoritest.WaitForSnapshot(t, w, 2)
	// version 3: B changes to b1, landing BEFORE any pin exists. Version 2's
	// retained snapshot (A=a1, B=b0) is now already stale in field B
	// relative to live, even though nothing has been pinned yet.
	p.Set("cfg/b", "b1")
	mamoritest.WaitForSnapshot(t, w, 3)

	// Drain the two ordinary Change events the unpinned setup flushes above
	// (version 2 and version 3) already delivered: this test is about the
	// single coalesced Change Unpin produces, not those, and left undrained
	// they would be misread as the Unpin event below (same channel, FIFO).
	for i := 0; i < 2; i++ {
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatal("did not receive expected setup Change event")
		}
	}

	if err := w.Pin(2); err != nil {
		t.Fatalf("Pin(2): %v", err)
	}
	if got := w.Get(); got.A != "a1" || got.B != "b0" {
		t.Fatalf("Get() after Pin(2) = %+v, want {a1 b0}", got)
	}

	// A further change while pinned: version 4, A changes again to a2. B
	// does not change again after this point, so its earlier (pre-pin)
	// change is the only thing that must surface it in the coalesced diff.
	p.Set("cfg/a", "a2")
	waitForLive(t, w, 4)

	w.Unpin()

	var ev mamori.Change[config]
	select {
	case ev = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("Unpin did not deliver a Change")
	}

	if ev.Old.A != "a1" || ev.Old.B != "b0" {
		t.Fatalf("Change.Old = %+v, want the pinned snapshot {a1 b0}", ev.Old)
	}
	if ev.New.A != "a2" || ev.New.B != "b1" {
		t.Fatalf("Change.New = %+v, want the latest live values {a2 b1}", ev.New)
	}

	// The core assertion: B genuinely differs between Old and New (b0 vs
	// b1) even though it did not change WHILE pinned, only before Pin(2)
	// was called. Both A and B must be named in Fields, and the count must
	// be exactly 2, not 1.
	if len(ev.Fields) != 2 {
		t.Fatalf("coalesced Change has %d fields, want 2 (A and B): %+v", len(ev.Fields), ev.Fields)
	}
	if !ev.Changed("A") {
		t.Fatalf("Change.Changed(\"A\") = false, want true: %+v", ev.Fields)
	}
	if !ev.Changed("B") {
		t.Fatalf("Change.Changed(\"B\") = false, want true (B differs between the pinned config and live even though it changed before Pin was called): %+v", ev.Fields)
	}
}

// TestPinConcurrentWithReconcileRace drives Pin/Unpin/Status/Get from one
// goroutine while another goroutine keeps pushing provider changes that the
// reconciler goroutine applies, run under -race. It asserts nothing about
// values; the point is that none of this is a data race.
func TestPinConcurrentWithReconcileRace(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-race")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-race://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.Set("cfg/level", "v"+strconv.Itoa(i))
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			w.PinCurrent()
			_ = w.Status()
			_ = w.Get()
			_, _ = w.Pinned()
			w.Unpin()
		}
	}()

	wg.Wait()
}

// TestPinThenCloseNoDeadlockNoLeak proves the guarantee Close depends on:
// sendPin selects on <-w.ctx.Done(), and Close's cancel() does not return
// until that channel is closed, so a Pin call racing Close always has a way
// out (errWatcherClosed) instead of blocking on a control channel nothing
// will ever receive from again. Without that guarantee this test would hang
// instead of failing cleanly, which is why it is bounded by an explicit
// timeout rather than just calling Close directly.
func TestPinThenCloseNoDeadlockNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-close")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-close://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	w.PinCurrent()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.Pin(1) // races Close; must return, not hang, either way
	}()

	done := make(chan struct{})
	go func() {
		_ = w.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s: a concurrent Pin appears to have deadlocked it")
	}
	wg.Wait()
}

// TestPinAfterParentContextCancel proves the Bug 1 fix: the reconciler's loop
// (see engine.loop) exits on <-ctx.Done() for TWO independent reasons, not
// just Close -- the parent context passed to Watch being cancelled directly
// is an equally supported shutdown path, and neither Close nor its w.closed
// signaling is involved when that happens. Before the fix, sendPin's select
// only had a way out via w.closed (closed solely by Close) and the control
// send itself (which has no receiver once loop has returned), so a
// PinCurrent/Pin/Unpin call made after a bare parent-context cancellation --
// without ever calling Close -- would block forever. After the fix, sendPin
// also selects on <-w.ctx.Done(), which is the same context the parent
// cancellation propagates into, so it always has a way out once the
// reconciler goroutine is gone, regardless of why.
//
// This test hangs (and fails on its own timeout) against the pre-fix code,
// and passes after the fix: it is not vacuous.
func TestPinAfterParentContextCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-parent-cancel")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-parent-cancel://cfg/level"`
	}

	parentCtx, cancel := context.WithCancel(context.Background())
	w, err := mamori.Watch[config](parentCtx,
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	cancel() // reconciler loop exits on wctx.Done(); Close is never called.

	// Give the reconciler goroutine time to actually return from loop, so
	// the reproduction is deterministic: control has no receiver left, not
	// a race against the goroutine's own exit.
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Pin(1); err == nil {
			t.Error("Pin() after parent context cancellation = nil error, want non-nil (watcher's reconciler is gone)")
		}
		_ = w.PinCurrent()
		w.Unpin()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pin/PinCurrent/Unpin did not return within 2s after the parent context was cancelled: sendPin appears to have deadlocked")
	}
}

// TestPinnedReconcileRecordsRefresh is the regression test for the Bug 2
// fix: RecordRefresh must fire for a value reconciled WHILE PINNED, not only
// from the unpinned flush path, since Meter.RecordRefresh is documented
// (observ.go) as "a watched value changed and was reconciled" -- which is
// exactly what happens to Live/lastGood on every flush regardless of pin
// state. Before the fix, RecordRefresh was called only from the unpinned
// branch of flush, so reconciliations coalesced during a pin never
// incremented the counter.
//
// It also checks the no-double-count half of the fix: applying the
// coalesced result at Unpin time must not call RecordRefresh again for the
// same fields, since Unpin delivers an already-reconciled snapshot rather
// than reconciling anew.
func TestPinnedReconcileRecordsRefresh(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-pin-metrics")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-pin-metrics://cfg/level"`
	}

	m := &fakeMeter{}
	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithMeter(m))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.PinCurrent(); got != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", got)
	}

	before := m.refreshes.Load()

	// This flush lands entirely while pinned: buildCandidate/flush still
	// run (Live advances), but Get and OnChange stay frozen. waitForLive
	// returning only after flush has both advanced e.version and published
	// the new report guarantees flush (and any RecordRefresh call it made)
	// has already completed by the time we read the counter below.
	p.Set("cfg/level", "v1")
	waitForLive(t, w, 2)

	after := m.refreshes.Load()
	if after <= before {
		t.Fatalf("RecordRefresh call count while pinned = %d, want > %d (a pinned reconcile must still be metered)", after, before)
	}

	// Get and OnChange stay frozen (pin.go's contract) even though the
	// refresh underneath was metered.
	if got := w.Get().Level; got != "v0" {
		t.Fatalf("Get().Level while pinned = %q, want v0 (frozen)", got)
	}

	// Unpin applies the already-reconciled snapshot to Get and emits the
	// coalesced Change; it must not meter that as a fresh reconciliation.
	// sendPin (which Unpin uses) does not return until handlePin has
	// finished and replied, so the counter read immediately after is not
	// racing handlePin's own work.
	beforeUnpin := m.refreshes.Load()
	w.Unpin()
	afterUnpin := m.refreshes.Load()
	if afterUnpin != beforeUnpin {
		t.Fatalf("RecordRefresh call count changed by Unpin: %d -> %d, want unchanged (Unpin must not double-count)", beforeUnpin, afterUnpin)
	}
}
