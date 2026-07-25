package mamori_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// This file covers Task 3 (chain watching): the engine watches every
// position of a field's precedence chain and recomputes the winning value on
// any source update.
//
// Mechanism note for the live-takeover test: the brief suggests either
// t.Setenv for a real env: position or a second mamoritest.Provider. env: is
// not a WatchableProvider (see builtin_env.go), so driving it at runtime
// means polling on a FakeClock, and there is no way to cleanly *unset* an
// env var mid-test with t.Setenv (it only ever sets a value, restoring the
// original at cleanup). Two mamoritest.Provider instances give native,
// instantaneous delivery for BOTH the "set" and the "unset" (Del) half of
// the takeover, so that is the mechanism used below.

// assertNoFurtherEvent drains events, failing the test if one more arrives
// within a short grace window. It is the same pattern watch_semantics_test.go
// (TestWatchCoalescing) uses to prove a batch of changes coalesced into
// exactly one Change.
func assertNoFurtherEvent[T any](t *testing.T, events chan mamori.Change[T]) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("unexpected extra Change event: %+v", ev.Fields)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchChainLiveTakeoverAndFallback(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwta")
	b := mamoritest.NewProvider("chwtb")
	b.Set("port", "8080") // A absent at startup; B must win.

	type cfg struct {
		Port string `source:"chwta://port,chwtb://port"`
	}

	events := make(chan mamori.Change[cfg], 8)
	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(a), mamori.WithProvider(b),
		mamori.OnChange(func(ev mamori.Change[cfg]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().Port; got != "8080" {
		t.Fatalf("initial Port = %q, want 8080 (B wins, A absent)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("initial Snapshot = %d, want 1", got)
	}

	// Exporting the higher-precedence source at runtime must take over: one
	// Change, new value from A.
	a.Set("port", "9090")
	mamoritest.WaitForSnapshot(t, w, 2)
	if got := w.Get().Port; got != "9090" {
		t.Fatalf("Port after A set = %q, want 9090 (A must take over)", got)
	}
	select {
	case ev := <-events:
		if ev.New.Port != "9090" || ev.Old.Port != "8080" {
			t.Fatalf("takeover Change = %+v, want Old=8080 New=9090", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Change delivered for the takeover")
	}
	assertNoFurtherEvent(t, events)

	// Unsetting A must fall back to B: one Change, back to B's value.
	a.Del("port")
	mamoritest.WaitForSnapshot(t, w, 3)
	if got := w.Get().Port; got != "8080" {
		t.Fatalf("Port after A del = %q, want 8080 (fall back to B)", got)
	}
	select {
	case ev := <-events:
		if ev.New.Port != "8080" || ev.Old.Port != "9090" {
			t.Fatalf("fallback Change = %+v, want Old=9090 New=8080", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Change delivered for the fallback")
	}
	assertNoFurtherEvent(t, events)
}

func TestWatchChainLowerPriorityChangeDoesNotAffectWinner(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwlpa")
	b := mamoritest.NewProvider("chwlpb")
	a.Set("port", "valA")
	b.Set("port", "valB1")

	type cfg struct {
		Port string `source:"chwlpa://port,chwlpb://port"`
	}

	events := make(chan mamori.Change[cfg], 4)
	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(a), mamori.WithProvider(b),
		mamori.OnChange(func(ev mamori.Change[cfg]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().Port; got != "valA" {
		t.Fatalf("initial Port = %q, want valA (higher precedence wins)", got)
	}

	// A change to the LOWER-priority position must be observed (no error)
	// but must not move Get(): A still wins.
	b.Set("port", "valB2")
	time.Sleep(150 * time.Millisecond)

	if got := w.Get().Port; got != "valA" {
		t.Fatalf("Port after B changed = %q, want valA unchanged (A still wins)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d after a non-winning position changed, want 1", got)
	}
	rep := w.Status()
	if !rep.Healthy {
		t.Fatalf("Status unhealthy after a benign lower-priority change: %+v", rep)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected Change from a non-winning position update: %+v", ev.Fields)
	default:
	}
}

func TestWatchChainStatusReportsWinningRef(t *testing.T) {
	// Parity with Doctor (doctor.go): Status/Report should name the ref
	// actually in effect for a chain, not always the first/primary ref.
	a := mamoritest.NewProvider("chwrefa")
	b := mamoritest.NewProvider("chwrefb")
	b.Set("port", "8080") // A absent: B wins.

	type cfg struct {
		Port string `source:"chwrefa://port,chwrefb://port"`
	}

	w, err := mamori.Watch[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if len(rep.Fields) != 1 {
		t.Fatalf("Status reported %d fields, want 1", len(rep.Fields))
	}
	if got := rep.Fields[0].Scheme; got != "chwrefb" {
		t.Fatalf("Scheme = %q, want chwrefb (the winning ref, not chwrefa)", got)
	}
}

func TestWatchChainOnFailKeepLastRetainsValueAndDeliversError(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwka")
	b := mamoritest.NewProvider("chwkb")
	a.Set("x", "valA")
	b.Set("x", "valB")

	type cfg struct {
		V string `source:"chwka://x,chwkb://x"`
	}

	opt, capture := mamoritest.CaptureErrors()
	w, err := mamori.Watch[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b), opt)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().V; got != "valA" {
		t.Fatalf("initial V = %q, want valA", got)
	}

	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	_ = mamoritest.WaitForError(t, capture, mamori.KindPermissionDenied)

	// Precedence, not failover: even though B has a value ready, the chain
	// must not slide to it. keeplast retains A's last value.
	if got := w.Get().V; got != "valA" {
		t.Fatalf("V after A errors = %q, want valA retained (keeplast, no failover to B)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d on a keeplast error, want 1 (no candidate applied)", got)
	}
	rep := w.Status()
	if rep.Healthy {
		t.Fatalf("Status healthy despite an active permission-denied error: %+v", rep)
	}
	if err := w.Health(); err == nil {
		t.Fatal("Health() = nil after a keeplast error, want non-nil")
	}
}

func TestWatchChainOnFailDefaultAppliesDefaultSilently(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwda")
	b := mamoritest.NewProvider("chwdb")
	a.Set("x", "valA")
	b.Set("x", "valB")

	type cfg struct {
		V string `source:"chwda://x,chwdb://x" default:"fallback" onfail:"default"`
	}

	errs := make(chan error, 4)
	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(a), mamori.WithProvider(b),
		mamori.OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().V; got != "valA" {
		t.Fatalf("initial V = %q, want valA", got)
	}

	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().V; got != "fallback" {
		t.Fatalf("V after A errors = %q, want fallback (onfail:default applied)", got)
	}

	// onfail:"default" masks the error silently, exactly like the one-shot
	// Load path's onFailUseDefault: no OnError delivery.
	select {
	case e := <-errs:
		t.Fatalf("unexpected OnError delivery for onfail:default: %v", e)
	default:
	}
}

func TestWatchChainOnFailFailRejectsWholeCandidate(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwfa")
	b := mamoritest.NewProvider("chwfb")
	c := mamoritest.NewProvider("chwfc")
	a.Set("x", "valA")
	b.Set("x", "valB")
	c.Set("y", "o1")

	type cfg struct {
		A     string `source:"chwfa://x,chwfb://x" default:"fallback" onfail:"fail"`
		Other string `source:"chwfc://y"`
	}

	opt, capture := mamoritest.CaptureErrors()
	w, err := mamori.Watch[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b), mamori.WithProvider(c), opt)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().A; got != "valA" {
		t.Fatalf("initial A = %q, want valA", got)
	}

	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	_ = mamoritest.WaitForError(t, capture, mamori.KindPermissionDenied)

	// A stays put (candidate rejected, not applied).
	time.Sleep(100 * time.Millisecond)
	if got := w.Get().A; got != "valA" {
		t.Fatalf("A after fail-policy error = %q, want valA (unset, rejected candidate)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d while blocked, want 1", got)
	}

	// While blocked, an otherwise-legitimate change to an unrelated field
	// must ALSO be rejected: onfail:"fail" rejects the whole candidate, not
	// just the failing field.
	c.Set("y", "o2")
	time.Sleep(150 * time.Millisecond)
	if got := w.Get().Other; got != "o1" {
		t.Fatalf("Other = %q after Other changed while blocked, want o1 (whole candidate rejected)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d while blocked, want 1", got)
	}

	// Once the blocking field resolves again (a new value), everything that
	// was held back applies together.
	a.Set("x", "valA2")
	mamoritest.WaitForSnapshot(t, w, 2)
	if got := w.Get().A; got != "valA2" {
		t.Fatalf("A after recovery = %q, want valA2", got)
	}
	if got := w.Get().Other; got != "o2" {
		t.Fatalf("Other after recovery = %q, want o2 (the held-back change now applies)", got)
	}
}

// TestWatchSingleRefHonorsOnFailAtRuntime supersedes the old
// TestWatchChainSingleRefIgnoresOnFailAtRuntime, which asserted the WRONG
// (pre-fix) behavior: that a one-element chain forced onfail:"keeplast" at
// runtime no matter what the field's onfail tag said. The runtime recompute
// no longer special-cases len(Refs) == 1; it applies the field's OnFail
// policy uniformly, exactly as applyOnFail (resolve.go) already does on the
// Load path. This test proves the onfail:"fail" half of that: a single-ref
// field with onfail:"fail" now rejects the whole candidate on a runtime
// non-not-found error, exactly as a multi-ref chain with the same tag does
// (TestWatchChainOnFailFailRejectsWholeCandidate).
//
// TestWatchSingleRefOnFailDefaultAppliesAtRuntime (below) covers the
// onfail:"default" half. TestWatchBackoffRetainsLastGood
// (watch_semantics_test.go) is the companion proof that an UNTAGGED
// single-ref field is unaffected by this fix: it uses no onfail tag and
// still retains the last-good value on a runtime error, delivering the
// error to OnError, exactly as before (the single-ref invariant).
func TestWatchSingleRefHonorsOnFailAtRuntime(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("chwsrf")
	c := mamoritest.NewProvider("chwsrfc")
	p.Set("x", "initial")
	c.Set("y", "o1")

	type cfg struct {
		V     string `source:"chwsrf://x" default:"fallback" onfail:"fail"`
		Other string `source:"chwsrfc://y"`
	}

	opt, capture := mamoritest.CaptureErrors()
	w, err := mamori.Watch[cfg](context.Background(), mamori.WithProvider(p), mamori.WithProvider(c), opt)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().V; got != "initial" {
		t.Fatalf("initial V = %q, want initial", got)
	}

	p.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	_ = mamoritest.WaitForError(t, capture, mamori.KindPermissionDenied)

	// V stays put: the candidate was rejected outright, not applied and not
	// silently masked behind "fallback" either (that would be onfail:default,
	// a different tag).
	time.Sleep(100 * time.Millisecond)
	if got := w.Get().V; got != "initial" {
		t.Fatalf("V after onfail:fail runtime error = %q, want initial (candidate rejected)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d while blocked, want 1", got)
	}

	// While blocked, an otherwise-legitimate change to an unrelated field
	// must ALSO be rejected: onfail:"fail" rejects the whole candidate, not
	// just the failing field (same contract as the multi-ref chain case).
	c.Set("y", "o2")
	time.Sleep(150 * time.Millisecond)
	if got := w.Get().Other; got != "o1" {
		t.Fatalf("Other = %q after Other changed while blocked, want o1 (whole candidate rejected)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d while blocked, want 1", got)
	}

	// Once the blocking field resolves again, everything held back applies
	// together.
	p.Set("x", "recovered")
	mamoritest.WaitForSnapshot(t, w, 2)
	if got := w.Get().V; got != "recovered" {
		t.Fatalf("V after recovery = %q, want recovered", got)
	}
	if got := w.Get().Other; got != "o2" {
		t.Fatalf("Other after recovery = %q, want o2 (the held-back change now applies)", got)
	}
}

// TestWatchSingleRefOnFailDefaultAppliesAtRuntime is the onfail:"default"
// counterpart to TestWatchSingleRefHonorsOnFailAtRuntime: a single-ref field
// with onfail:"default" now applies its default value on a runtime
// non-not-found error, exactly as a multi-ref chain with the same tag does
// (TestWatchChainOnFailDefaultAppliesDefaultSilently) and exactly as
// applyOnFail (resolve.go) already does for this same field on the Load
// path - masking the error silently, with no OnError delivery.
func TestWatchSingleRefOnFailDefaultAppliesAtRuntime(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("chwsrd")
	p.Set("x", "initial")

	type cfg struct {
		V string `source:"chwsrd://x" default:"fallback" onfail:"default"`
	}

	errs := make(chan error, 4)
	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(p),
		mamori.OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().V; got != "initial" {
		t.Fatalf("initial V = %q, want initial", got)
	}

	p.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().V; got != "fallback" {
		t.Fatalf("V after onfail:default runtime error = %q, want fallback (onfail:default applied)", got)
	}

	// onfail:"default" masks the error silently, exactly like the one-shot
	// Load path's onFailUseDefault: no OnError delivery.
	select {
	case e := <-errs:
		t.Fatalf("unexpected OnError delivery for onfail:default: %v", e)
	default:
	}
}

func TestWatchChainGoleakManyPositions(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwgla")
	b := mamoritest.NewProvider("chwglb")
	c := mamoritest.NewProvider("chwglc")
	c.Set("x", "valC") // only the lowest-priority position has a value.

	type cfg struct {
		V string `source:"chwgla://x,chwglb://x,chwglc://x"`
	}

	w, err := mamori.Watch[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b), mamori.WithProvider(c))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if got := w.Get().V; got != "valC" {
		t.Fatalf("initial V = %q, want valC", got)
	}
	// Touch every position once before closing, so every forwarder
	// goroutine has actually delivered at least one update.
	a.Set("x", "valA")
	b.Set("x", "valB")
	c.Set("x", "valC2")
	time.Sleep(100 * time.Millisecond)

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWatchChainConcurrentCrossPositionUpdatesRace(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwraca")
	b := mamoritest.NewProvider("chwracb")
	a.Set("x", "a0")
	b.Set("x", "b0")

	type cfg struct {
		V string `source:"chwraca://x,chwracb://x" default:"fallback"`
	}

	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(a), mamori.WithProvider(b),
		mamori.WithDebounce(5*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	var wg sync.WaitGroup
	const iterations = 100
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.Set("x", fmt.Sprintf("a%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			b.Set("x", fmt.Sprintf("b%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.Del("x")
			a.Set("x", fmt.Sprintf("a%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = w.Get()
			_ = w.Status()
		}
	}()
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWatchChainOnFailFailUnblockRearmsHeldBackChange reproduces the gap
// where lifting an onfail:"fail" block did not re-arm a flush for an
// unrelated field whose change was held back while blocked.
//
// Sequence: A (onfail:"fail") errors, blocking every field's flush
// (TestWatchChainOnFailFailRejectsWholeCandidate already proves that half).
// While still blocked, Other changes o1 -> o2: the new value lands in
// e.observed, but the flush attempt is rejected (buildCandidate sees A
// blocked) and, crucially, pending is cleared regardless of that rejection
// - this test waits past the debounce window so that rejection has actually
// happened, not merely not-yet-fired. A then RECOVERS TO THE EXACT SAME
// VALUE it held before the error (the ordinary transient-outage case:
// availability returned, the value itself never changed). Because
// mamoritest's Provider versions are content-hashed, this recovery is
// byte-for-byte the same Value/Version A had before the block, so
// markChanged's own change-detection is a no-op for A: nothing about
// processing A's recovery would arm a timer on its own without the fix.
//
// Pre-fix, this leaves Other stranded at o1 with no timer pending and no
// error surfaced, so WaitForSnapshot(t, w, 2) below times out and fails the
// test - proving this test is non-vacuous. Post-fix, lifting the block
// re-arms a flush for Other (whose observed version already differs from
// what's applied), and Other's held-back o2 applies within the wait.
func TestWatchChainOnFailFailUnblockRearmsHeldBackChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := mamoritest.NewProvider("chwfra")
	c := mamoritest.NewProvider("chwfrc")
	a.Set("x", "valA")
	c.Set("y", "o1")

	type cfg struct {
		A     string `source:"chwfra://x" default:"fallback" onfail:"fail"`
		Other string `source:"chwfrc://y"`
	}

	opt, capture := mamoritest.CaptureErrors()
	events := make(chan mamori.Change[cfg], 8)
	w, err := mamori.Watch[cfg](context.Background(),
		mamori.WithProvider(a), mamori.WithProvider(c),
		mamori.WithDebounce(20*time.Millisecond),
		mamori.OnChange(func(ev mamori.Change[cfg]) { events <- ev }),
		opt,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().A; got != "valA" {
		t.Fatalf("initial A = %q, want valA", got)
	}

	// A errors: onfail:"fail" blocks every field's flush.
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	_ = mamoritest.WaitForError(t, capture, mamori.KindPermissionDenied)

	// Other changes while A is blocked. Sleep well past the (shortened)
	// debounce window so the held-back flush attempt actually fires and gets
	// rejected - and pending actually gets cleared - rather than merely
	// still sitting unfired.
	c.Set("y", "o2")
	time.Sleep(150 * time.Millisecond)
	if got := w.Get().Other; got != "o1" {
		t.Fatalf("Other = %q after change while blocked, want o1 (held back)", got)
	}
	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("Snapshot advanced to %d while blocked, want 1", got)
	}

	// A recovers to the exact same value/version it had before the error.
	a.Clear("x")

	// Deterministic, bounded wait: without the fix, nothing re-arms a flush
	// for Other and this times out.
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().A; got != "valA" {
		t.Fatalf("A after recovery = %q, want valA (unchanged)", got)
	}
	if got := w.Get().Other; got != "o2" {
		t.Fatalf("Other after recovery = %q, want o2 (the held-back change now applies)", got)
	}

	select {
	case ev := <-events:
		if ev.Changed("A") {
			t.Fatalf("unexpected Change for A (recovered to the same value): %+v", ev.Fields)
		}
		if !ev.Changed("Other") {
			t.Fatalf("Change did not report Other as changed: %+v", ev.Fields)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Change delivered for the unblocked flush")
	}

	// Exactly one flush applied: no duplicate or follow-on Change.
	assertNoFurtherEvent(t, events)
	if got := w.Status().Snapshot; got != 2 {
		t.Fatalf("final Snapshot = %d, want 2 (exactly one flush)", got)
	}
}
