package mamori

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

type deriveCfg struct {
	A string
	B string
}

func TestWithDeriveRegistersInOrder(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { c.A = "first"; return nil })(o)
	WithDerive(func(c *deriveCfg) error { c.A += "-second"; return nil })(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("got %d derives, want 2", len(fns))
	}

	var cfg deriveCfg
	for _, fn := range fns {
		if err := fn.fn(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if cfg.A != "first-second" {
		t.Fatalf("got %q, want %q: derives must run in registration order", cfg.A, "first-second")
	}
}

// TestWithDeriveNoWritesIsBackwardCompatible is the backward-compatibility
// pin: WithDerive(fn), with no writes declared at all, must keep compiling
// (this call site has no third argument) and must keep behaving exactly as
// it did before writes existed - the hook still registers, still runs, and
// reports no writes. A subtly wrong implementation that made writes
// effectively required (for example, by treating a zero-length writes as an
// error, or by requiring at least one call site in the package to pass one)
// would fail this test either at compile time (the call below has no third
// argument) or at the typedDerives/fn.fn checks below.
func TestWithDeriveNoWritesIsBackwardCompatible(t *testing.T) {
	o := &options{}
	ran := false
	WithDerive(func(c *deriveCfg) error { ran = true; return nil })(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 1 {
		t.Fatalf("got %d derives, want 1", len(fns))
	}
	if len(fns[0].writes) != 0 {
		t.Fatalf("got writes %v, want none for WithDerive(fn) with no paths declared", fns[0].writes)
	}
	var cfg deriveCfg
	if err := fns[0].fn(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("WithDerive(fn) with no writes must still register and run its hook")
	}
}

// TestWithDeriveRecordsSingleWrite: WithDerive(fn, "DSN") must record writes
// as ["DSN"].
func TestWithDeriveRecordsSingleWrite(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "DSN")(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 1 {
		t.Fatalf("got %d derives, want 1", len(fns))
	}
	want := []string{"DSN"}
	if !slices.Equal(fns[0].writes, want) {
		t.Fatalf("got writes %v, want %v", fns[0].writes, want)
	}
}

// TestWithDeriveRecordsMultipleWritesInOrder: WithDerive(fn, "DSN",
// "RedisURL") must record both paths, in the order given.
func TestWithDeriveRecordsMultipleWritesInOrder(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "DSN", "RedisURL")(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 1 {
		t.Fatalf("got %d derives, want 1", len(fns))
	}
	want := []string{"DSN", "RedisURL"}
	if !slices.Equal(fns[0].writes, want) {
		t.Fatalf("got writes %v, want %v in order", fns[0].writes, want)
	}
}

// TestWithDeriveMultipleHooksKeepOwnWrites: two WithDerive calls must each
// keep their own declared writes, and registration order must be preserved
// (mirroring TestWithDeriveRegistersInOrder's ordering guarantee, now for
// writes as well as behavior).
func TestWithDeriveMultipleHooksKeepOwnWrites(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "A")(o)
	WithDerive(func(c *deriveCfg) error { return nil }, "B", "C")(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("got %d derives, want 2", len(fns))
	}
	if !slices.Equal(fns[0].writes, []string{"A"}) {
		t.Fatalf("first hook: got writes %v, want [A]", fns[0].writes)
	}
	if !slices.Equal(fns[1].writes, []string{"B", "C"}) {
		t.Fatalf("second hook: got writes %v, want [B C]", fns[1].writes)
	}
}

// TestTypedDerivesRejectsEmptyWritePath: an empty write path is exactly the
// invisible-field problem writes exists to fix, so it must be rejected loudly
// rather than silently ignored.
func TestTypedDerivesRejectsEmptyWritePath(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "")(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error for an empty write path")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
}

// TestTypedDerivesRejectsWhitespaceOnlyWritePath: whitespace-only is the same
// invisible-field problem in disguise (it never matches a real field path
// either), so it is rejected the same way an empty path is.
func TestTypedDerivesRejectsWhitespaceOnlyWritePath(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "   ")(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error for a whitespace-only write path")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
}

// TestTypedDerivesRejectsBadWritePathNamesHookPosition: the error must name
// which WithDerive call is at fault, not just that some call is, so the
// mistake is findable when a caller has registered more than one hook.
func TestTypedDerivesRejectsBadWritePathNamesHookPosition(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { return nil }, "A")(o)
	WithDerive(func(c *deriveCfg) error { return nil }, "")(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error for the second hook's empty write path")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("error must name the offending hook's position (index 1, the second WithDerive call), got: %v", err)
	}
}

// TestLoadRejectsDeriveWithEmptyWritePath proves the empty-path rejection
// happens at Load time (through typedDerives, called from loadValue), not
// merely when typedDerives is called directly. dsnCfg's env vars are
// deliberately left unset: the write-path check runs before resolveAll (see
// loadValue, reconcile.go), so this must fail on the bad write path, not on a
// resolve failure - mirroring how
// TestDeriveTypeMismatchOnLoadReportsHookErrorNotResolveError proves the same
// ordering for a mismatched hook type.
func TestLoadRejectsDeriveWithEmptyWritePath(t *testing.T) {
	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error { return nil }, ""),
	)
	if err == nil {
		t.Fatal("Load must reject a WithDerive hook with an empty write path")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
}

func TestTypedDerivesNoneIsNil(t *testing.T) {
	fns, err := typedDerives[deriveCfg](&options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fns != nil {
		t.Fatalf("got %v, want nil when no derive is registered", fns)
	}
}

// TestWithDeriveNilHookIsNoop guards WithDerive's `if fn == nil { return }`
// check, protective behavior neither PreApply nor OnChange has, so no sibling
// test holds it in place. Without the guard this test does not merely fail:
// nil stored into o.derives as `any` still satisfies typedDerives's type
// assertion (the dynamic type matches; only the value is nil), so the derives
// loop in loadValue goes on to call a nil func value and panics with a nil
// pointer dereference - on the Watch path that panic runs inside the
// reconciler goroutine, a process crash rather than a returned error. See
// WithDerive's own doc comment for why silently dropping a nil hook is a
// deliberate clamp, not an oversight.
func TestWithDeriveNilHookIsNoop(t *testing.T) {
	o := &options{}
	WithDerive[deriveCfg](nil)(o)
	if len(o.derives) != 0 {
		t.Fatalf("got %d derives installed for a nil hook, want 0", len(o.derives))
	}

	if _, err := Load[deriveCfg](context.Background(), WithDerive[deriveCfg](nil)); err != nil {
		t.Fatalf("Load with only a nil WithDerive hook must succeed exactly as it would with no WithDerive at all, got %v", err)
	}
}

// A mismatched type parameter must be a loud error, not a silent no-op. This is
// the property that distinguishes WithDerive from OnChange, which discards the
// failed assertion and then never fires. See the DeriveError doc comment.
func TestTypedDerivesTypeMismatchIsLoud(t *testing.T) {
	type otherCfg struct{ Z string }

	o := &options{}
	WithDerive(func(c *otherCfg) error { return nil })(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error when the derive's type parameter does not match")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
	for _, want := range []string{"func(*mamori.otherCfg) error", "func(*mamori.deriveCfg) error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the mistake is findable, got: %v", want, err)
		}
	}
}

func TestDeriveErrorUnwraps(t *testing.T) {
	base := errors.New("boom")
	de := &DeriveError{Err: base}

	if !errors.Is(de, base) {
		t.Error("DeriveError must unwrap to its cause")
	}
	if !strings.Contains(de.Error(), "boom") {
		t.Errorf("DeriveError message must carry the cause, got %q", de.Error())
	}
}

type dsnCfg struct {
	User string `source:"env:DERIVE_USER"`
	Pass string `source:"env:DERIVE_PASS"`
	DSN  string
}

func TestDeriveRunsOnLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")

	cfg, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error {
			c.DSN = c.User + ":" + c.Pass
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DSN != "alice:s3cret" {
		t.Fatalf("got %q, want %q", cfg.DSN, "alice:s3cret")
	}
}

type multiDeriveCfg struct {
	First    string `source:"env:DERIVE_FIRST"`
	Last     string `source:"env:DERIVE_LAST"`
	FullName string
	Greeting string
}

// TestDeriveMultipleHooksComposeThroughLoad is the pipeline-level counterpart
// to TestWithDeriveRegistersInOrder: that test calls typedDerives directly and
// invokes the returned funcs by hand, below the pipeline, so nothing before
// this test ever registered two WithDerive hooks through the actual Load or
// Watch entry point and let loadValue run them in order. This does, matching
// the "field derived from another derived field" example on
// site/src/pages/docs/usage/derived-fields.md: the second hook must see the
// first hook's output, because it runs after it, through the full Load path
// (decode -> derive loop -> validate), not a hand-rolled substitute for it.
func TestDeriveMultipleHooksComposeThroughLoad(t *testing.T) {
	t.Setenv("DERIVE_FIRST", "Ada")
	t.Setenv("DERIVE_LAST", "Lovelace")

	cfg, err := Load[multiDeriveCfg](context.Background(),
		WithDerive(func(c *multiDeriveCfg) error {
			c.FullName = c.First + " " + c.Last
			return nil
		}),
		WithDerive(func(c *multiDeriveCfg) error {
			c.Greeting = "Hello, " + c.FullName
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FullName != "Ada Lovelace" {
		t.Fatalf("FullName = %q, want %q", cfg.FullName, "Ada Lovelace")
	}
	if cfg.Greeting != "Hello, Ada Lovelace" {
		t.Fatalf("Greeting = %q, want the second hook to see the first hook's output through the full Load pipeline", cfg.Greeting)
	}
}

// The load-bearing ordering test: a derived field carrying a validate tag must
// be validated on its DERIVED value, not on the zero value it held a moment
// earlier. If derive ran after validation this would fail.
type validatedDeriveCfg struct {
	User string `source:"env:DERIVE_USER"`
	DSN  string `validate:"required"`
}

func TestDeriveRunsBeforeValidation(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")

	if _, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { c.DSN = "postgres://" + c.User; return nil }),
	); err != nil {
		t.Fatalf("a derive that fills a required field must satisfy validation, got %v", err)
	}

	_, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a derive that leaves a required field empty must fail validation")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %T, want *ValidationError", err)
	}
}

func TestDeriveErrorFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	boom := errors.New("boom")

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error { return boom }),
	)
	if err == nil {
		t.Fatal("a derive returning an error must fail the Load")
	}
	var de *DeriveError
	if !errors.As(err, &de) {
		t.Fatalf("got %T, want *DeriveError", err)
	}
	if !errors.Is(err, boom) {
		t.Error("the cause must survive in the chain")
	}
}

func TestDeriveTypeMismatchFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	type otherCfg struct{ Z string }

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *otherCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a mismatched derive type must fail Load, not be silently skipped")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want an error satisfying ErrInvalid", err)
	}
}

// mismatchedDeriveResolveCfg has a field with no registered provider, so
// resolveAll always fails for it - the "failing resolve" half of the scenario
// below. It carries no validate tag and no default, so the resolve failure is
// the only way this type can fail to load.
type mismatchedDeriveResolveCfg struct {
	X string `source:"nosuchscheme-derive-mismatch://x"`
}

// TestDeriveTypeMismatchOnLoadReportsHookErrorNotResolveError is the
// regression test for the ordering bug the whole-branch review found:
// typedPreApply is checked at the very top of loadValue, before any provider
// round trip, for exactly the reason its own comment gives - a caller bug
// must fail loudly and immediately, not after fields have already been
// resolved. typedDerives used to run only after resolveAll and buildInto, so
// a mismatched WithDerive hook combined with a field that fails to resolve
// reported the RESOLVE error, masking the caller's own type mistake and
// paying for a full round of (real, in production) provider round trips
// first. This asserts loadValue now catches the mismatch before resolveAll
// ever runs, matching typedPreApply's own documented position and the claim
// site/src/pages/docs/usage/derived-fields.md makes about a mismatched
// derive failing "the same way a mismatched PreApply does".
func TestDeriveTypeMismatchOnLoadReportsHookErrorNotResolveError(t *testing.T) {
	type otherCfg struct{ Z string }

	_, err := Load[mismatchedDeriveResolveCfg](context.Background(),
		WithDerive(func(c *otherCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("want an error: the field has no registered provider and the derive is mismatched")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying ErrInvalid (the derive hook type mismatch), not the resolve failure", err)
	}
	if strings.Contains(err.Error(), "no provider registered") {
		t.Fatalf("got the resolve error %q; want the derive hook type mismatch caught before any provider round trip is spent", err)
	}
}

// TestMeterCountsDeriveRejection covers buildCandidate's derive-hook rejection
// branch, mirrored from TestMeterCountsValidationRejection and
// TestMeterCountsPreApplyRejection in observ_test.go: RejectDerive is this
// task's named deliverable, and until this test existed nothing exercised it
// - deleting the RecordApplyRejected(RejectDerive) call in reconciler.go left
// `go test .` fully green.
func TestMeterCountsDeriveRejection(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("mderive")
	wp.set("a", "first", "v1")
	m := &recordingMeter{}
	reject := errors.New("derive boom")

	type Config struct {
		A string `source:"mderive://a"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
		WithDerive(func(c *Config) error {
			if c.A == "second" {
				return reject
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "one RejectDerive recorded", func() bool {
		return len(m.rejections()) == 1
	})

	got := m.rejections()
	if len(got) != 1 || got[0] != RejectDerive {
		t.Errorf("rejections = %v, want [%v]", got, RejectDerive)
	}
}

// TestLogsDeriveRejected covers buildCandidate's derive-hook rejection log
// line, mirrored from TestLogsValidationRejected and TestLogsPreApplyRejected
// in logging_test.go. Like the meter counter above, nothing exercised this
// log call before this test: deleting it also left `go test .` green.
func TestLogsDeriveRejected(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lderive")
	wp.set("a", "first", "v1")
	h, logger := newRecorder()
	reject := errors.New("derive boom")

	type Config struct {
		A string `source:"lderive://a"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
		WithDerive(func(c *Config) error {
			if c.A == "second" {
				return reject
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "derive-rejected record", func() bool {
		_, ok := h.find("candidate rejected by a derive hook; continuing to serve the previous config")
		return ok
	})

	r, _ := h.find("candidate rejected by a derive hook; continuing to serve the previous config")
	if r.Level != slog.LevelError {
		t.Errorf("level = %v, want Error", r.Level)
	}
}

// TestBuildCandidateDeriveClearsMarkWhenHookPanics mirrors
// TestRunPreApplyClearsMarkWhenHookPanics (preapply_reentrancy_test.go) for
// the derive loop in buildCandidate: a derive hook occupies the identical
// inline position on the reconciler goroutine that a PreApply hook does (see
// the derive loop's own doc comment in reconciler.go and armReentrancy in
// preapply.go), so a derive that panics must not leave e.w.inCallback set
// either.
//
// It is written directly against buildCandidate rather than a live Watcher for
// the same reason the PreApply version is: a panicking hook is not survivable
// on the real path today (nothing in this package recovers), so there is no
// "and then a later Pin still works" left to observe on a process that no
// longer exists. This is the regression test for the commit that added the
// derive loop to armReentrancy's list of call sites while making it the only
// one of the three that disarmed with explicit calls instead of a deferred
// one - restore that explicit-disarm version and this test fails, because the
// panic propagates out of the derive loop's for-range before either explicit
// disarm() call is reached.
func TestBuildCandidateDeriveClearsMarkWhenHookPanics(t *testing.T) {
	type cfg struct{ A string }

	o := defaultOptions()
	w := &Watcher[cfg]{}
	e := &engine[cfg]{
		o: o,
		w: w,
		derives: []typedDerive[cfg]{
			{fn: func(*cfg) error { panic("derive blew up") }},
		},
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("buildCandidate swallowed the derive hook's panic; it must propagate")
			}
		}()
		_, _, _ = e.buildCandidate()
	}()

	if got := w.inCallback.Load(); got != 0 {
		t.Errorf("inCallback = %d after a panicking derive hook, want 0: a mark left set would reject every later pin command from the reconciler goroutine", got)
	}
}
