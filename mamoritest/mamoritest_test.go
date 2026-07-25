package mamoritest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// waitFor polls cond every 5ms until it returns true or timeout elapses,
// failing the test if the deadline is reached first. Used to observe an
// async effect of a Set/Del/Fail/Clear call landing on a real mamori.Watch
// without a fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// TestLoadResolvesSetValue verifies the most basic contract: a value staged
// with Set is what a real mamori.Load resolves for a matching ref.
func TestLoadResolvesSetValue(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-load")
	p.Set("cfg/level", "debug")

	type config struct {
		Level string `source:"mt-load://cfg/level"`
	}

	cfg, err := mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Level != "debug" {
		t.Fatalf("Level = %q, want debug", cfg.Level)
	}
}

// TestWatchObservesSetChange drives a real mamori.Watch and confirms it picks
// up a value pushed by Set: the whole point of mamoritest is that this
// happens over the native watch channel Provider.Watch hands back, not by
// polling the backend. It waits for the change to land with WaitForSnapshot
// rather than an inline poll, exercising the helper the way an application
// test is expected to.
func TestWatchObservesSetChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-watch")
	p.Set("cfg/level", "info")

	type config struct {
		Level string `source:"mt-watch://cfg/level" default:"fallback"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.Get().Level != "info" {
		t.Fatalf("initial Level = %q, want info", w.Get().Level)
	}

	p.Set("cfg/level", "debug")

	mamoritest.WaitForSnapshot(t, w, 2)
	if w.Get().Level != "debug" {
		t.Fatalf("Get().Level = %q after Set, want debug", w.Get().Level)
	}
}

// TestDelRequiredFieldFails verifies Del makes a subsequent Load of a
// required (no default, not optional) field fail with mamori.ErrNotFound,
// the same way an absent key does at Load time.
func TestDelRequiredFieldFails(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-del-req")
	p.Set("required", "present")

	type config struct {
		Required string `source:"mt-del-req://required"`
	}

	if _, err := mamori.Load[config](context.Background(), mamori.WithProvider(p)); err != nil {
		t.Fatalf("Load before Del: %v", err)
	}

	p.Del("required")

	_, err := mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err == nil {
		t.Fatal("Load after Del = nil error, want one wrapping ErrNotFound")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Load after Del error = %v, want errors.Is(err, mamori.ErrNotFound)", err)
	}
}

// TestDelDefaultedFieldFallsBack verifies Del on a field with a `default:`
// tag does not fail Load: it falls back to the default, mirroring how
// reconciler.go's handleErr now tolerates ErrNotFound for default/optional
// fields at runtime, and how resolveAll/applyDefault already tolerates it at
// Load time.
func TestDelDefaultedFieldFallsBack(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-del-def")
	p.Set("defaulted", "present")

	type config struct {
		Defaulted string `source:"mt-del-def://defaulted" default:"fallback"`
	}

	cfg, err := mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Load before Del: %v", err)
	}
	if cfg.Defaulted != "present" {
		t.Fatalf("Defaulted before Del = %q, want present", cfg.Defaulted)
	}

	p.Del("defaulted")

	cfg, err = mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Load after Del: %v", err)
	}
	if cfg.Defaulted != "fallback" {
		t.Fatalf("Defaulted after Del = %q, want fallback", cfg.Defaulted)
	}
}

// TestFailAndClear verifies Fail makes Resolve (via a real Load) return the
// injected error, and Clear restores normal resolution.
func TestFailAndClear(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-fail")
	p.Set("level", "info")

	type config struct {
		Level string `source:"mt-fail://level"`
	}

	if _, err := mamori.Load[config](context.Background(), mamori.WithProvider(p)); err != nil {
		t.Fatalf("Load before Fail: %v", err)
	}

	boom := errors.New("boom: backend unavailable")
	p.Fail("level", boom)

	_, err := mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err == nil {
		t.Fatal("Load after Fail = nil error, want one wrapping the injected error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Load after Fail error = %v, want errors.Is(err, boom)", err)
	}

	p.Clear("level")

	cfg, err := mamori.Load[config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Load after Clear: %v", err)
	}
	if cfg.Level != "info" {
		t.Fatalf("Level after Clear = %q, want info", cfg.Level)
	}
}

// TestWatchFailDeliversErrorAndClearRecovers exercises Fail/Clear over a real
// Watch's native channel (as opposed to TestFailAndClear, which only drives
// one-shot Loads): Fail must surface as an OnError callback and Clear must
// bring the watcher back to healthy without ever emitting a bad value.
func TestWatchFailDeliversErrorAndClearRecovers(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-watch-fail")
	p.Set("level", "info")

	type config struct {
		Level string `source:"mt-watch-fail://level"`
	}

	errs := make(chan error, 4)
	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0),
		mamori.OnError(func(e error) { errs <- e }))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if err := w.Health(); err != nil {
		t.Fatalf("initial Health = %v, want nil", err)
	}

	// Wrapping mamori.ErrPermissionDenied (rather than a bare error) makes this
	// a terminal error kind (see fieldUnhealthy in report.go), so Health
	// actually flips - a bare error only classifies as KindUnknown, which
	// fieldUnhealthy does not treat as unhealthy on its own.
	boom := fmt.Errorf("%w: backend unavailable", mamori.ErrPermissionDenied)
	p.Fail("level", boom)

	select {
	case e := <-errs:
		if !errors.Is(e, boom) {
			t.Fatalf("OnError delivered %v, want errors.Is(err, boom)", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnError did not fire after Fail")
	}
	if w.Get().Level != "info" {
		t.Fatalf("Get().Level after Fail = %q, want last-good info", w.Get().Level)
	}
	waitFor(t, 2*time.Second, "watcher to become unhealthy after Fail", func() bool {
		return w.Health() != nil
	})

	p.Clear("level")

	waitFor(t, 2*time.Second, "watcher to become healthy again after Clear", func() bool {
		return w.Health() == nil
	})
	if w.Get().Level != "info" {
		t.Fatalf("Get().Level after Clear = %q, want info", w.Get().Level)
	}
}

// TestWatchCloseNoGoroutineLeak drives a Provider through a full Set/Del/Fail/
// Clear cycle under a real Watch, then closes it, and lets the deferred
// goleak.VerifyNone (which runs after this function returns) confirm neither
// the Watcher's own goroutines nor Provider.Watch's deregistration goroutine
// are left running.
func TestWatchCloseNoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-leak")
	p.Set("a", "1")
	p.Set("b", "2")

	type config struct {
		A string `source:"mt-leak://a"`
		B string `source:"mt-leak://b" default:"b-default"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Safety net: if a waitFor below fails the test early, this still closes
	// the watcher so the deferred goleak check reports a real leak (if any)
	// instead of goroutines that simply never got a chance to unwind.
	defer func() { _ = w.Close() }()

	before := w.Status().Snapshot
	p.Set("a", "1-updated")
	waitFor(t, 2*time.Second, "snapshot to advance after Set", func() bool {
		return w.Status().Snapshot != before
	})
	if w.Get().A != "1-updated" {
		t.Fatalf("Get().A = %q after Set, want 1-updated", w.Get().A)
	}

	// A genuine (non-not-found) terminal-kind error on field b makes the
	// watcher unhealthy (see fieldUnhealthy in report.go; a bare error would
	// classify as KindUnknown, which does not affect health on its own)...
	p.Fail("b", fmt.Errorf("%w: backend unavailable", mamori.ErrPermissionDenied))
	waitFor(t, 2*time.Second, "watcher to become unhealthy after Fail", func() bool {
		return w.Health() != nil
	})

	// ...and Del's ErrNotFound on that same defaulted field is tolerated by
	// the reconciler (reconciler.go's handleErr), restoring health. The
	// field's last-observed value is retained rather than reset to the
	// default - re-applying a default at runtime is explicitly out of scope
	// for that tolerance, per handleErr's comment - so this only asserts on
	// health; TestDelDefaultedFieldFallsBack covers the Load-time
	// fallback-to-default behavior.
	p.Del("b")
	waitFor(t, 2*time.Second, "watcher to become healthy again after a tolerated Del", func() bool {
		return w.Health() == nil
	})

	p.Set("b", "2-again")
	p.Clear("b")

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWaitForSnapshotObservesChange verifies WaitForSnapshot blocks a test
// until a Set has actually been applied by a real mamori.Watcher, then lets it
// proceed: no sleep in the test body, and Get reflects the new value the
// instant WaitForSnapshot returns. The initial load is snapshot version 1, so
// the first applied change lands on version 2 (see reconciler.go).
func TestWaitForSnapshotObservesChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-wait-snapshot")
	p.Set("db/password", "hunter2")

	type Config struct {
		Pw string `source:"mt-wait-snapshot://db/password"`
	}

	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("db/password", "rotated")
	mamoritest.WaitForSnapshot(t, w, 2)
	if got := w.Get().Pw; got != "rotated" {
		t.Fatalf("after WaitForSnapshot, Get().Pw = %q, want rotated", got)
	}
}

// TestWaitForErrorCapturesKind verifies CaptureErrors' OnError sink and
// WaitForError together let a test assert that a provider-side failure
// reaches OnError classified with the expected Kind, without a raw channel or
// a hand-rolled poll in the test body.
func TestWaitForErrorCapturesKind(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-wait-error")
	p.Set("k", "v")

	type Config struct {
		K string `source:"mt-wait-error://k"`
	}

	onErr, errs := mamoritest.CaptureErrors()
	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p), onErr)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Fail("k", mamori.ErrPermissionDenied)
	got := mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
	if mamori.ErrorKind(got) != mamori.KindPermissionDenied {
		t.Fatalf("WaitForError returned kind %q, want permission_denied", mamori.ErrorKind(got))
	}
}

// TestWaitForSnapshotTimesOutCleanly proves WaitForSnapshot fails the test
// (rather than blocking forever, or worse, silently returning) when the
// requested snapshot version never arrives. It drives WaitForSnapshot with a
// recordingTB standing in for testing.T, so the expected failure is recorded
// rather than actually failing this test - the same technique
// providertest_test.go uses to test RunErrorClassification's own failure
// paths.
func TestWaitForSnapshotTimesOutCleanly(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-wait-timeout")
	p.Set("k", "v")

	type Config struct {
		K string `source:"mt-wait-timeout://k"`
	}

	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	fake := &recordingTB{TB: t}
	// Version 1_000_000 is never going to arrive: nothing ever Sets a change
	// that many times, so this exercises the deadline path.
	mamoritest.WaitForSnapshot(fake, w, 1_000_000)
	if !fake.failed {
		t.Fatal("WaitForSnapshot did not fail the test when the target version never arrived")
	}
}

// TestWatchConcurrentSetConvergesToLatest is a regression test for a fixed
// Provider.Watch bug: the baseline Update used to be sent to a new
// subscription's channel AFTER the subscription was appended to p.subs and
// p.mu released. That left a window where a Set running concurrently with
// Watch's registration could see the newly-registered subscription and
// enqueue the new value before the baseline send happened, producing channel
// order [new-value, baseline]. The reconciler applies updates strictly in
// channel-arrival order (reconciler.go's loop unconditionally overwrites
// e.observed[path] on every update it receives), so the stale baseline would
// land last and Get() would regress to it instead of converging to the value
// actually Set. Provider.Watch now computes and sends the baseline while
// still holding p.mu, before the subscription is appended to p.subs, so a
// concurrent Set's publish (which also needs p.mu, via snapshotSubs) can
// never observe the subscription before the baseline is already enqueued.
//
// The window is timing-dependent, and once fixed it cannot be forced open
// deterministically from this external test package: doing so would require
// instrumenting Provider.Watch itself, which would defeat the point of
// testing the fix as shipped. This test instead races many independent
// Watch/Set pairs, maximizing the chance that at least one iteration would
// have hit the window against a reintroduced version of the bug; run with
// -race, so any data race the reordering could cause is also caught
// directly rather than relying on the value assertion alone.
func TestWatchConcurrentSetConvergesToLatest(t *testing.T) {
	defer goleak.VerifyNone(t)

	type config struct {
		K string `source:"mt-race://k"`
	}

	const iterations = 50
	for i := 0; i < iterations; i++ {
		p := mamoritest.NewProvider("mt-race")
		p.Set("k", "baseline")

		start := make(chan struct{})
		setDone := make(chan struct{})
		go func() {
			<-start
			p.Set("k", "updated")
			close(setDone)
		}()

		// Signal the racer and immediately establish the Watch: Provider.Watch
		// is called synchronously inside mamori.Watch, so this is as close as
		// an external caller can get to racing the Set against the moment the
		// subscription is registered.
		close(start)
		w, err := mamori.Watch[config](context.Background(),
			mamori.WithProvider(p), mamori.WithDebounce(0))
		if err != nil {
			t.Fatalf("iteration %d: Watch: %v", i, err)
		}

		<-setDone

		waitFor(t, 2*time.Second,
			fmt.Sprintf("iteration %d: Get to converge to the value Set concurrently with Watch", i),
			func() bool { return w.Get().K == "updated" })

		// Give the reconciler a moment to drain any trailing update, then
		// re-check: a buggy channel order of [updated, baseline] applies
		// "updated" first and "baseline" last, so the convergence check above
		// could sample in between and miss the regression back to baseline.
		time.Sleep(15 * time.Millisecond)
		if got := w.Get().K; got != "updated" {
			t.Fatalf("iteration %d: Get().K = %q after converging once, want updated (regressed back to baseline)", i, got)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
	}
}

// recordingTB captures whether a call failed the (fake) test, without failing
// the real enclosing test. It embeds testing.TB (which has an unexported
// method, so it must be embedded rather than reimplemented) solely to satisfy
// the interface; every method the Wait helpers actually call is overridden
// below to record state and return normally. Unlike the real *testing.T,
// Fatalf here does not stop execution (it does not call runtime.Goexit), which
// is why WaitForSnapshot and WaitForError return immediately after calling it
// rather than relying on that to happen implicitly. Modeled on
// providertest_test.go's recordingTB.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(format string, args ...any) { r.failed = true }
func (r *recordingTB) Fatalf(format string, args ...any) { r.failed = true }
func (r *recordingTB) Fatal(args ...any)                 { r.failed = true }
func (r *recordingTB) Error(args ...any)                 { r.failed = true }
func (r *recordingTB) Helper()                           {}
