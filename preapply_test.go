package mamori

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestPreApplyOptionStoresTypedFunc(t *testing.T) {
	type cfg struct{ A string }
	o := defaultOptions()
	PreApply(func(context.Context, Change[cfg]) error { return nil })(o)
	if o.preApply == nil {
		t.Fatal("PreApply did not store the hook")
	}
	if _, ok := o.preApply.(func(context.Context, Change[cfg]) error); !ok {
		t.Fatalf("stored hook has type %T, want func(context.Context, Change[cfg]) error", o.preApply)
	}
}

func TestPreApplyTimeoutDefaultAndOverride(t *testing.T) {
	o := defaultOptions()
	if o.preApplyTimeout != defaultPreApplyTimeout {
		t.Errorf("default = %v, want %v", o.preApplyTimeout, defaultPreApplyTimeout)
	}
	WithPreApplyTimeout(3 * time.Second)(o)
	if o.preApplyTimeout != 3*time.Second {
		t.Errorf("after override = %v, want 3s", o.preApplyTimeout)
	}
}

// TestPreApplyTimeoutClampsNonPositive pins the clamp, and the reason it is a
// clamp rather than an honored value.
//
// context.WithTimeout(parent, 0) returns a context that is ALREADY past its
// deadline, so runPreApply's post-hook ctx.Err() check would refuse every
// candidate a hook ever saw - the initial load included, which makes Watch and
// Load fail outright at startup. The rest of this package spells a disabling
// zero (WithStale) and clamps nonsense input (WithHistory's negative n), so
// neither reading a caller could plausibly have in mind matches what honoring it
// would do.
func TestPreApplyTimeoutClampsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		o := defaultOptions()
		WithPreApplyTimeout(d)(o)
		if o.preApplyTimeout != defaultPreApplyTimeout {
			t.Errorf("WithPreApplyTimeout(%v) = %v, want the default %v", d, o.preApplyTimeout, defaultPreApplyTimeout)
		}
	}
}

// TestPreApplyTimeoutZeroStillGatesTheInitialLoad is the behavioral half of the
// clamp: without it, WithPreApplyTimeout(0) turns a hook that returns nil into a
// rejection of the very first configuration, and Watch never returns a watcher
// at all.
func TestPreApplyTimeoutZeroStillGatesTheInitialLoad(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pz://a"`
	}
	p := newWatchProvider("pz")
	p.set("a", "first", "v1")

	var calls atomic.Int64
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
		WithPreApplyTimeout(0),
		PreApply(func(ctx context.Context, _ Change[cfg]) error {
			calls.Add(1)
			return ctx.Err() // an already-expired budget would surface here
		}),
	)
	if err != nil {
		t.Fatalf("Watch with WithPreApplyTimeout(0) = %v, want a working watcher: a zero budget must clamp to the default, not reject everything", err)
	}
	defer func() { _ = w.Close() }()

	if got := calls.Load(); got != 1 {
		t.Errorf("hook ran %d times on the initial load, want 1", got)
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first", got)
	}
}

func TestPreApplyErrorWrapsAndReports(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := &PreApplyError{Err: cause}
	if !errors.Is(err, cause) {
		t.Error("PreApplyError must unwrap to its cause")
	}
	if got := err.Error(); got == "" {
		t.Error("PreApplyError.Error() must not be empty")
	}
	var pe *PreApplyError
	if !errors.As(error(err), &pe) {
		t.Error("errors.As must reach *PreApplyError")
	}
}

// preApplyOtherConfig is a second config type, declared at package scope so
// the type name in the mismatch error below is stable and worth asserting on.
type preApplyOtherConfig struct{ B string }

// TestPreApplyWrongTypeFailsWatch pins the one place a mistyped hook can be
// caught. Option is untyped, so a hook written against a different config type
// compiles cleanly and the assertion in Watch yields nil - and a nil gate is an
// open gate. Tolerating it the way onChange tolerates its own mismatch would
// mean the hook is never called, nothing is ever delivered to OnError, Status
// stays healthy, and every rotation is applied unverified: the exact failure
// PreApply exists to prevent, arrived at by installing PreApply. Watch must
// refuse to start instead.
func TestPreApplyWrongTypeFailsWatch(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pw://a"`
	}
	p := newWatchProvider("pw")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[preApplyOtherConfig]) error {
			return errors.New("refuse everything")
		}),
	)
	if err == nil {
		_ = w.Close()
		t.Fatal("Watch accepted a PreApply hook typed for a different config: the gate would be silently open")
	}
	if w != nil {
		t.Errorf("Watch returned a non-nil Watcher alongside its error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want one wrapping ErrInvalid", err)
	}
	// The message has to name both types: "PreApply hook has the wrong type" is
	// useless when the two candidates are two similarly-named config structs.
	for _, want := range []string{"PreApply", "preApplyOtherConfig", "cfg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPreApplyRejectionKeepsLastGood pins the core contract: a rejected
// candidate leaves Get, OnChange, and the served config exactly as they were.
func TestPreApplyRejectionKeepsLastGood(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pa://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("pa")
	p.set("a", "first", "v1")

	changed := make(chan cfg, 4)
	errs := make(chan error, 4)
	reject := errors.New("credential does not work")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "second" {
				return reject
			}
			return nil
		}),
		OnChange(func(ev Change[cfg]) { changed <- ev.New }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case e := <-errs:
		if !errors.Is(e, reject) {
			t.Errorf("error = %v, want one wrapping the hook's error", e)
		}
		var pe *PreApplyError
		if !errors.As(e, &pe) {
			t.Errorf("error = %v, want a *PreApplyError", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error delivered for a rejected candidate")
	}
	select {
	case ev := <-changed:
		t.Fatalf("OnChange fired for a rejected candidate: %+v", ev)
	default:
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first (last good must survive a rejection)", got)
	}
}

// TestPreApplyRejectionIsRetriedOnNextChange is the regression test for the
// e.applied rollback. buildCandidate advances e.applied as a side effect, so a
// rejection that does not roll it back leaves every rejected field looking
// already-applied: the next flush computes an empty diff and the value is
// never retried, with Get silently serving stale config forever.
//
// Here the hook rejects "bad" and accepts anything else. Recovery to "good"
// alone does NOT pin the rollback: without it the engine has already recorded
// v2 as applied, so v3's diff still computes and the recovery happens anyway.
// What separates the two worlds is the OldVersion the recovery's own diff
// carries. It is the engine reporting, in the only place e.applied is
// observable from outside, which version it believes it last applied: v1 when
// the rejected v2 was rolled back, v2 when it was left advanced - the exact
// wrong belief that strands a value that arrives without a further version
// bump behind it.
func TestPreApplyRejectionIsRetriedOnNextChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pr://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("pr")
	p.set("a", "first", "v1")

	changed := make(chan Change[cfg], 4)
	errs := make(chan error, 4)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "bad" {
				return errors.New("rejected")
			}
			return nil
		}),
		OnChange(func(ev Change[cfg]) { changed <- ev }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "bad", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection for the bad value")
	}

	// The rejected version must not be recorded as applied: a good value now
	// has to be applied, and its diff has to start from the last version that
	// really was applied.
	p.push("a", "good", "v3")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case got := <-changed:
		if got.New.A != "good" {
			t.Errorf("after recovery A = %q, want good", got.New.A)
		}
		if len(got.Fields) != 1 {
			t.Fatalf("recovery Fields = %+v, want exactly one entry for A", got.Fields)
		}
		if got.Fields[0].OldVersion != "v1" {
			t.Errorf("recovery OldVersion = %q, want v1: the rejected v2 was never applied, "+
				"so leaving e.applied advanced past it is exactly the staleness bug",
				got.Fields[0].OldVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not recover after a rejection")
	}
	if got := w.Get().A; got != "good" {
		t.Errorf("Get().A = %q, want good", got)
	}
	select {
	case e := <-errs:
		t.Errorf("second error after a clean recovery: %v", e)
	default:
	}
}

// TestPreApplyRejectedValueIsStillGatedOnALaterFlush is the other half of the
// rollback, and the one with teeth: a rejected value is NOT withdrawn from
// e.observed, so it stays in every candidate built afterwards. Leaving
// e.applied advanced hides it from the very next diff, and a hook written the
// way PreApply's own documentation recommends - verify only what Changed -
// then waves the whole candidate through on some unrelated field's flush. The
// rejected credential reaches Get without anything ever having verified it,
// which is worse than the staleness the rollback's other half prevents.
func TestPreApplyRejectedValueIsStillGatedOnALaterFlush(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"px://a"`
		B string `source:"px://b"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("px")
	p.set("a", "first", "a1")
	p.set("b", "first", "b1")

	changed := make(chan Change[cfg], 4)
	errs := make(chan error, 4)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if !ev.Changed("A") {
				return nil // the documented "only check what actually rotated" shape
			}
			if ev.New.A == "bad" {
				return errors.New("credential does not work")
			}
			return nil
		}),
		OnChange(func(ev Change[cfg]) { changed <- ev }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "bad", "a2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection for the bad value")
	}

	// An unrelated field changes. A is still "bad" and still unverified, so
	// this candidate must still be refused - which only happens if A is still
	// in the diff the hook is handed.
	p.push("b", "second", "b2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case ev := <-changed:
		t.Fatalf("an unrelated field's flush applied the still-rejected A=%q; "+
			"the gate never saw it because Fields = %+v", ev.New.A, ev.Fields)
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection for the candidate still carrying the bad value")
	}
	if got := w.Get(); got.A != "first" || got.B != "first" {
		t.Errorf("Get() = %+v, want both fields at their last good values", got)
	}

	// A recovers. B's change was held back by the rejection, not lost, so the
	// single Change that lands now must carry both fields.
	p.push("a", "good", "a3")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case ev := <-changed:
		if ev.New.A != "good" || ev.New.B != "second" {
			t.Errorf("recovered config = %+v, want A=good B=second", ev.New)
		}
		if !ev.Changed("A") || !ev.Changed("B") {
			t.Errorf("recovery Fields = %+v, want both A and B: B's held-back change must "+
				"not be stranded by A's rejection", ev.Fields)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not recover after a rejection")
	}
}

// TestPreApplyRejectionDoesNotAdvanceTheSnapshot pins the other half of the
// rejection contract: no version is burned on a candidate that never became
// current, so Snapshot and Live both stay where they were and Pin(version)
// cannot later reach a refused snapshot.
//
// It deliberately does NOT carry the rollback's name. An earlier draft called
// it TestPreApplyRollsBackAppliedVersions, which was a lie: it passes with the
// rollback deleted, because a refused flush returns before e.version++ either
// way. The rollback is pinned by TestPreApplyRejectionIsRetriedOnNextChange
// and TestPreApplyRejectedValueIsStillGatedOnALaterFlush, both of which fail
// without it.
func TestPreApplyRejectionDoesNotAdvanceTheSnapshot(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pb://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("pb")
	p.set("a", "first", "v1")

	errs := make(chan error, 4)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "second" {
				return errors.New("no")
			}
			return nil
		}),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection")
	}

	// Status reports the field's observed version, and the snapshot version
	// must NOT have advanced past the last applied one.
	rep := w.Status()
	if rep.Snapshot != 1 {
		t.Errorf("Snapshot = %d, want 1 (a rejected candidate must not advance the served snapshot)", rep.Snapshot)
	}
	if rep.Live != 1 {
		t.Errorf("Live = %d, want 1 (a rejected candidate is not a reconciled snapshot at all)", rep.Live)
	}
}

// TestPreApplyGatesWhilePinned pins where the gate sits relative to flush's
// pinned branch. A pin freezes what Get returns; it does not stop the engine
// reconciling and advancing Live, and Unpin then applies the newest such
// snapshot wholesale, gating nothing itself. A gate that ran only on the
// unpinned branch would make Unpin the one path able to publish a candidate
// nothing ever verified - which is exactly the failure a rotation gate exists
// to prevent, arriving at the least expected moment.
func TestPreApplyGatesWhilePinned(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pp://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("pp")
	p.set("a", "first", "v1")

	changed := make(chan Change[cfg], 4)
	errs := make(chan error, 4)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "bad" {
				return errors.New("credential does not work")
			}
			return nil
		}),
		OnChange(func(ev Change[cfg]) { changed <- ev }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	w.PinCurrent()

	p.push("a", "bad", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("a pinned watcher still reconciles, so it must still gate what it reconciles")
	}
	if rep := w.Status(); rep.Live != 1 {
		t.Errorf("Live = %d, want 1: a refused candidate is not a reconciled snapshot, pinned or not", rep.Live)
	}

	w.Unpin()
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q after Unpin, want first: Unpin must never publish a candidate the gate refused", got)
	}
	select {
	case ev := <-changed:
		t.Fatalf("Unpin emitted a Change for a refused candidate: %+v", ev.Fields)
	default:
	}
}

// TestPreApplyTimeoutIsARejection pins decision D4.
func TestPreApplyTimeoutIsARejection(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pt://a"`
	}
	p := newWatchProvider("pt")
	p.set("a", "first", "v1")

	release := make(chan struct{})
	errs := make(chan error, 4)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		WithPreApplyTimeout(50*time.Millisecond),
		PreApply(func(ctx context.Context, ev Change[cfg]) error {
			if ev.New.A != "second" {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		}),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { close(release); _ = w.Close() }()

	p.push("a", "second", "v2")

	select {
	case e := <-errs:
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Errorf("error = %v, want one wrapping context.DeadlineExceeded", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hook that never returns must be rejected on timeout, not awaited")
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first (a timed-out hook must not apply)", got)
	}
}

// TestPreApplyNotCalledWhenNothingChanged guards against spending a network
// round trip on every poll tick.
func TestPreApplyNotCalledWhenNothingChanged(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pn://a"`
	}
	p := newWatchProvider("pn")
	p.set("a", "only", "v1")

	var calls atomic.Int32
	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[cfg]) error {
			calls.Add(1)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Re-push the identical version: buildCandidate computes an empty diff and
	// must not reach the gate at all.
	before := calls.Load()
	p.push("a", "only", "v1")
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != before {
		t.Errorf("hook called %d extra times for an unchanged value, want 0", got-before)
	}
}

// TestPreApplyWrongTypeFailsLoad is Load's counterpart to
// TestPreApplyWrongTypeFailsWatch. loadValue is the shared load path behind
// both Load and Watch's initial resolve, and it gates the initial
// configuration itself (decision D7, see TestPreApplyRejectsInitialLoad below)
// - which means it needs the very same guard against a mistyped hook that
// Watch already has, not a bare comma-ok assertion of its own. A bare
// assertion would report a hook typed for the wrong config as "no hook
// installed" and return the unverified configuration as though the gate had
// approved it: Load's own fail-open version of the bug
// TestPreApplyWrongTypeFailsWatch already closed for Watch, and arguably
// worse, since it would run before the program has any configuration at all.
func TestPreApplyWrongTypeFailsLoad(t *testing.T) {
	type cfg struct {
		A string `source:"pv://a"`
	}
	p := newWatchProvider("pv")
	p.set("a", "first", "v1")

	_, err := Load[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[preApplyOtherConfig]) error {
			return errors.New("refuse everything")
		}),
	)
	if err == nil {
		t.Fatal("Load accepted a PreApply hook typed for a different config: the gate would be silently open")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want one wrapping ErrInvalid", err)
	}
	// Same message requirement as Watch's version: naming both types is the
	// only way the error is useful when the two candidates are similarly named.
	for _, want := range []string{"PreApply", "preApplyOtherConfig", "cfg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPreApplyRejectsInitialLoad pins decision D7: a hook that verifies a
// credential should verify the first one too, because discovering at startup
// that the configured credential does not work beats discovering it at the
// first rotation. Watch is already fail-fast on its initial Load for every
// other kind of error (resolve, validation); a PreApply rejection now joins
// that list rather than being the one failure mode Watch tolerates silently.
func TestPreApplyRejectsInitialLoad(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pi://a"`
	}
	p := newWatchProvider("pi")
	p.set("a", "bad", "v1")
	boom := errors.New("initial credential does not work")

	_, err := Watch[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[cfg]) error { return boom }),
	)
	if err == nil {
		t.Fatal("Watch must fail when PreApply rejects the initial configuration")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want one wrapping the hook's error", err)
	}
	var pe *PreApplyError
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want a *PreApplyError", err)
	}
}

// TestPreApplyRejectsLoad is TestPreApplyRejectsInitialLoad's one-shot
// counterpart: Load shares loadValue with Watch's initial resolve, so the
// same gate has to reject a one-shot Load exactly the way it rejects Watch's
// startup load.
func TestPreApplyRejectsLoad(t *testing.T) {
	type cfg struct {
		A string `source:"pl://a"`
	}
	p := newWatchProvider("pl")
	p.set("a", "bad", "v1")

	_, err := Load[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[cfg]) error {
			return errors.New("nope")
		}),
	)
	if err == nil {
		t.Fatal("Load must fail when PreApply rejects")
	}
	var pe *PreApplyError
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want a *PreApplyError", err)
	}
}

// TestPreApplyInitialLoadReceivesZeroOld pins what the initial Change looks
// like on this path: Old is the zero value of T, since nothing was serving
// yet, and New is the freshly loaded configuration. See
// TestPreApplyInitialLoadPopulatesFields for the other half of the initial
// Change, Fields, which is populated rather than left nil.
func TestPreApplyInitialLoadReceivesZeroOld(t *testing.T) {
	type cfg struct {
		A string `source:"pz://a"`
	}
	p := newWatchProvider("pz")
	p.set("a", "value", "v1")

	var gotOld, gotNew string
	_, err := Load[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			gotOld, gotNew = ev.Old.A, ev.New.A
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotOld != "" {
		t.Errorf("ev.Old.A = %q on the initial load, want the zero value", gotOld)
	}
	if gotNew != "value" {
		t.Errorf("ev.New.A = %q, want value", gotNew)
	}
}

// TestPreApplyInitialLoadPopulatesFields pins the other half of the initial
// Change: Fields is populated, not left nil, by applying the engine's own
// diff rule (buildCandidate, reconciler.go) against the true prior state at
// this point in the program's life. e.applied does not exist yet - Watch only
// seeds it after loadValue returns - so the prior version for every resolved
// field is the empty string, exactly what a missing e.applied entry already
// means everywhere else in this codebase (see flush's own comment on that
// equivalence in reconciler.go). Applying that rule here is what makes
// ev.Changed(path) report true for a field set on the initial load.
//
// This is the test that makes decision D7 actually work for the usage the
// docs recommend: without Fields populated, the documented guard pattern -
// "if !ev.Changed(path) { return nil }" - would silently skip verification of
// the very first value, the same class of silent gate failure Task 3 closed
// for a mistyped hook, arrived at through the guard pattern PreApply's own
// docs teach instead.
func TestPreApplyInitialLoadPopulatesFields(t *testing.T) {
	type cfg struct {
		A string `source:"pf://a"`
	}
	p := newWatchProvider("pf")
	p.set("a", "value", "v1")

	var changed bool
	var fields []FieldChange
	_, err := Load[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			changed = ev.Changed("A")
			fields = ev.Fields
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !changed {
		t.Error(`ev.Changed("A") = false on the initial load, want true: the documented guard ` +
			"pattern must not skip startup verification")
	}
	if len(fields) != 1 {
		t.Fatalf("ev.Fields = %+v, want exactly one entry for A", fields)
	}
	if fields[0].Path != "A" {
		t.Errorf("fields[0].Path = %q, want A", fields[0].Path)
	}
	if fields[0].OldVersion != "" {
		t.Errorf("fields[0].OldVersion = %q, want empty (no prior version existed at this point)", fields[0].OldVersion)
	}
	if fields[0].NewVersion != "v1" {
		t.Errorf("fields[0].NewVersion = %q, want v1", fields[0].NewVersion)
	}
}

// TestPreApplyInitialLoadCallsHookExactlyOnce guards against double-gating
// Watch's initial resolve. loadValue is the single place that runs the gate
// for the initial configuration (see its doc comment); Watch stores loadValue's
// already-gated result straight into the engine without a gate of its own.
// Were Watch to also assert-and-call the hook itself around its call to
// loadValue, a rejection would surface as two OnError deliveries and every
// initial load would cost two round trips (e.g. two dials for a hook that
// pings a database) instead of one.
func TestPreApplyInitialLoadCallsHookExactlyOnce(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"psx://a"`
	}
	p := newWatchProvider("psx")
	p.set("a", "first", "v1")

	var calls atomic.Int32
	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		PreApply(func(context.Context, Change[cfg]) error {
			calls.Add(1)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Settle before asserting: a synchronous check right after Watch returns
	// would only catch a second call made inline, on the same call stack, and
	// would miss an asynchronous re-gate performed moments later by the
	// reconciler goroutine (e.g. a spurious flush of the just-seeded observed
	// state). No push happens here, so nothing should ever become pending -
	// this sleep exists purely to give that goroutine a window to misbehave in
	// before the count is read.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("hook called %d times on Watch's initial load, want exactly 1", got)
	}
}
