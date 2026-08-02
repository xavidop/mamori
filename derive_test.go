package mamori

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
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

// TestStatusReportsDeclaredDerivedField is Task 2's headline Status()
// assertion: a path declared via WithDerive(fn, "DSN") must appear in
// Status().Fields, carrying Derived: true and a non-empty Version, but no ref,
// scheme, staleness, or error - which is exactly what an operator needs to see
// that the field exists and is maintained, without mamori inventing a ref for
// a field that has none. Version is populated (derivedVersion) so an operator
// can still tell one derived value apart from another without mamori ever
// publishing the value itself; see TestDerivedFieldVersionChangesOnRotation
// for the rotation case and TestReportJSONNeverCarriesDerivedValue for the
// guarantee that this version is never a way back to the value.
func TestStatusReportsDeclaredDerivedField(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-status")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-status://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if len(rep.Fields) != 2 {
		t.Fatalf("Status().Fields has %d entries, want 2 (User + DSN): %+v", len(rep.Fields), rep.Fields)
	}
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("Status().Fields has no entry for DSN, want one for a declared derive write path: %+v", rep.Fields)
	}
	if !found.Derived {
		t.Error("DSN entry has Derived = false, want true")
	}
	if found.Ref != "" || found.Scheme != "" {
		t.Errorf("DSN entry carries Ref=%q Scheme=%q, want both empty: a derived field has no ref", found.Ref, found.Scheme)
	}
	if found.Stale || !found.LastOK.IsZero() || found.LastError != "" || found.LastKind != "" {
		t.Errorf("DSN entry carries staleness/error/LastOK state, want all zero: %+v", found)
	}
	if found.Version == "" {
		t.Error("DSN entry has empty Version, want a non-empty derivedVersion of the resolved DSN")
	}
}

// TestReportJSONNeverCarriesDerivedValue is the security-relevant assertion
// from the brief: the report is the admin endpoint's HTTP body, so a derived
// field's entry must never carry its value. This is pinned against the
// report's actual JSON encoding, not against struct fields, because the
// encoding is what a caller of the admin endpoint actually receives.
func TestReportJSONNeverCarriesDerivedValue(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-json")
	wp.set("user", "alice", "v1")

	const marker = "UNIQUE_DERIVED_MARKER_VALUE_98765"
	type Config struct {
		User string `source:"sderive-json://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = marker; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	b, err := json.Marshal(w.Status())
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), marker) {
		t.Fatalf("Report JSON carried the derived field's value: %s", b)
	}
}

// TestWatcherHealthyUnaffectedByDerivedField pins the brief's third
// constraint, narrowed to the case that stays true after fieldUnhealthy
// stopped short-circuiting on Derived: a Watcher.Status derived field still
// has no ref, so it can never be stale and can never carry a resolve error - a
// hook that fails rejects the whole candidate in buildCandidate, so a
// published config never contains a failed derive. The Doctor case, where a
// failing hook DOES make a report unhealthy, is covered separately by
// TestDoctorFailingHookIsUnhealthy (doctor_derive_test.go).
func TestWatcherHealthyUnaffectedByDerivedField(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-health")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-health://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if !rep.Healthy {
		t.Fatalf("Healthy = false with only a derived field present and no error, want true: %+v", rep)
	}
	if err := w.Health(); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}
}

// TestDerivedFieldChangedTrueAfterInputRotation is the headline diff
// behavior: DSN is declared as a WithDerive write path, so once User rotates
// and the derive rebuilds DSN to a genuinely different value, ev.Changed
// ("DSN") must be true.
func TestDerivedFieldChangedTrueAfterInputRotation(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-changed")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-changed://user"`
		DSN  string
	}
	events := make(chan Change[Config], 4)
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
		OnChange(func(ev Change[Config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	select {
	case ev := <-events:
		if !ev.Changed("DSN") {
			t.Errorf(`ev.Changed("DSN") = false, want true: the derived value changed from "postgres://alice" to "postgres://bob"`)
		}
		if !ev.Changed("User") {
			t.Error(`ev.Changed("User") = false, want true`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for the rotation")
	}
}

// TestDerivedFieldVersionChangesOnRotation is the end-to-end form of the
// reveal guard: a rotated password must move the derived DSN's reported
// version, or an operator comparing versions across replicas would see a
// stale credential as identical to a fresh one.
func TestDerivedFieldVersionChangesOnRotation(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-version")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-version://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	var before string
	for _, f := range w.Status().Fields {
		if f.Path == "DSN" {
			before = f.Version
		}
	}
	if before == "" {
		t.Fatal("DSN entry has empty Version before rotation, want non-empty")
	}

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	var after string
	var found bool
	for _, f := range w.Status().Fields {
		if f.Path == "DSN" {
			after = f.Version
			found = true
		}
	}
	if !found {
		t.Fatal("Status().Fields has no entry for DSN after rotation")
	}
	if after == "" {
		t.Fatal("DSN entry has empty Version after rotation, want non-empty")
	}
	if after == before {
		t.Fatalf("DSN entry Version unchanged across rotation (%q), want it to move with the derived value from %q to %q", before, "postgres://alice", "postgres://bob")
	}
}

// TestDerivedFieldChangedFalseWhenValueUnchanged is the mutation-catching
// counterpart to the test above: Aux rotates (which is enough to trigger a
// reconcile and rebuild every derive), but DSN is derived only from User,
// which never changes, so the rebuilt DSN is byte-identical to the one
// already applied. A subtly wrong implementation that reported a declared
// derive as changed on every flush - rather than only when its value
// actually moved - would fail this test.
func TestDerivedFieldChangedFalseWhenValueUnchanged(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-unchanged")
	wp.set("user", "alice", "v1")
	wp.set("aux", "x", "v1")

	type Config struct {
		User string `source:"sderive-unchanged://user"`
		Aux  string `source:"sderive-unchanged://aux"`
		DSN  string
	}
	events := make(chan Change[Config], 4)
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
		OnChange(func(ev Change[Config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("aux", "y", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	select {
	case ev := <-events:
		if !ev.Changed("Aux") {
			t.Error(`ev.Changed("Aux") = false, want true`)
		}
		if ev.Changed("DSN") {
			t.Errorf(`ev.Changed("DSN") = true, want false: User never changed, so the rebuilt DSN is byte-identical to what was already applied`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for the rotation")
	}
}

// TestUndeclaredDerivedFieldStillInvisible pins the backward-compatibility
// constraint: a derive that mutates a field but never declares it as a write
// path must keep behaving exactly as it always did - invisible to both
// Status() and the change diff - even though the mutation itself still
// happens.
func TestUndeclaredDerivedFieldStillInvisible(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-undeclared")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-undeclared://user"`
		DSN  string
	}
	events := make(chan Change[Config], 4)
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }), // no writes declared
		OnChange(func(ev Change[Config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if len(rep.Fields) != 1 {
		t.Fatalf("Status().Fields has %d entries, want 1 (User only): %+v", len(rep.Fields), rep.Fields)
	}
	for _, f := range rep.Fields {
		if f.Derived {
			t.Errorf("Status().Fields carries a Derived entry for an undeclared derive write: %+v", f)
		}
	}

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	select {
	case ev := <-events:
		if ev.Changed("DSN") {
			t.Error(`ev.Changed("DSN") = true for an undeclared derive write, want false`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for the rotation")
	}
}

// TestDerivedFieldAgreesOnInitialLoadAndReconcile is the regression test the
// brief calls for directly: the three FieldChange construction sites
// (loadValue's initial-load Change, buildCandidate's candidate diff, and
// diffApplied's Unpin diff) must agree about a declared derive, rather than
// only two of three implementing it - the exact defect class ("Resolve and
// ResolveBatch disagreed about not-found", "a 404 branch existed on one
// request path and not its sibling") that produced two Criticals earlier in
// this project. This exercises the first two sites end to end: PreApply
// receives the SAME Change loadValue builds for the initial resolve, so its
// ev.Changed("DSN") reflects the initial-load site; OnChange's ev.Changed
// ("DSN") after a rotation reflects buildCandidate's site. Both must report
// true. (TestDeriveDeclaredWriteSurvivesPinUnpin, in derive_watch_test.go,
// exercises the third site, diffApplied, the same way.)
func TestDerivedFieldAgreesOnInitialLoadAndReconcile(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-agree")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-agree://user"`
		DSN  string
	}

	var mu sync.Mutex
	var initialChanged bool
	events := make(chan Change[Config], 4)
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
		PreApply(func(_ context.Context, ev Change[Config]) error {
			mu.Lock()
			initialChanged = ev.Changed("DSN")
			mu.Unlock()
			return nil
		}),
		OnChange(func(ev Change[Config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	mu.Lock()
	gotInitial := initialChanged
	mu.Unlock()
	if !gotInitial {
		t.Fatal(`initial-load site (loadValue, reconcile.go) reported Changed("DSN") = false, want true: DSN was built from the zero value on the first load`)
	}

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	select {
	case ev := <-events:
		if !ev.Changed("DSN") {
			t.Fatal(`reconciler site (buildCandidate, reconciler.go) reported Changed("DSN") = false after a rotation, want true - disagreeing with the initial-load site, which reported true above`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for the rotation")
	}
}

// TestStatusOmitsPhantomDerivedPathForUnknownField is the regression test for
// a review finding: buildReport used to append a FieldStatus for every
// declared write path unconditionally, so a typo'd path ("DSNN") or one
// naming a nested struct that does not exist ("Nope.Deep") was published to
// Status() - and the admin HTTP body - as a phantom Derived row for a field
// that does not exist on T at all. That disagreed with derivedFieldChanges
// (reconciler.go), which already skips exactly this case for the diff, and
// with WithDerive's own godoc: "A path that matches nothing simply never
// reports as written, degrading to today's behavior rather than
// misbehaving." buildReport now gates the append on fieldByPath(e.lastGood,
// p) resolving.
func TestStatusOmitsPhantomDerivedPathForUnknownField(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-phantom")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-phantom://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil },
			"DSN", "DSNN", "Nope.Deep"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	for _, f := range rep.Fields {
		if f.Path == "DSNN" || f.Path == "Nope.Deep" {
			t.Errorf("Status().Fields published a phantom row for an unresolvable declared path: %+v", f)
		}
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Status().Fields has %d entries, want 2 (User + DSN only, no phantom rows): %+v", len(rep.Fields), rep.Fields)
	}
}

// unexportedDeriveCfg carries a real field that is not exported, so a derive
// declaring it as a write path exercises fieldByPath's CanInterface guard
// (buildReport, report.go; derivedFieldChanges, reconciler.go) rather than
// the "no such field at all" branch TestStatusOmitsPhantomDerivedPathForUnknownField
// covers: FieldByName matches "hidden" by name just as readily as an exported
// field, so without the CanInterface check the field would be found, and
// calling Interface() on it would panic.
type unexportedDeriveCfg struct {
	User   string `source:"sderive-unexported://user"`
	DSN    string
	hidden string
}

func TestStatusOmitsDerivedPathNamingUnexportedField(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-unexported")
	wp.set("user", "alice", "v1")

	w, err := Watch[unexportedDeriveCfg](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *unexportedDeriveCfg) error {
			c.DSN = "postgres://" + c.User
			c.hidden = "internal"
			return nil
		}, "DSN", "hidden"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	for _, f := range rep.Fields {
		if f.Path == "hidden" {
			t.Errorf("Status().Fields published a row for an unexported field: %+v", f)
		}
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Status().Fields has %d entries, want 2 (User + DSN only): %+v", len(rep.Fields), rep.Fields)
	}
}

// TestDerivedFieldChangeSkipsRefreshMetric is the regression test for a
// second review finding: a derived FieldChange used to flow into flush's
// RecordRefresh loop the same as a source field's, and schemeForPath returns
// "" for any path outside e.specs (which a derived path always is), so every
// declared-write rebuild recorded an empty-scheme refresh - a new, unlabeled
// mamori_refresh_total{scheme=""} series for every caller who declares a
// write path. isDerivedPath (reconciler.go) now skips a derived FieldChange
// in that loop entirely, deliberately, rather than fabricating a scheme
// label: see RecordRefresh's doc comment (observ.go) for why "a watched value
// changed" does not describe a derive rebuild in the first place.
func TestDerivedFieldChangeSkipsRefreshMetric(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-metric")
	wp.set("user", "alice", "v1")
	m := &recordingMeter{}

	type Config struct {
		User string `source:"sderive-metric://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	schemes := m.refreshSchemeList()
	if len(schemes) != 1 {
		t.Fatalf("RecordRefresh called %d times for a flush with one source field and one derived field, want exactly 1 (the derived rebuild must not record its own refresh): %v", len(schemes), schemes)
	}
	if schemes[0] != "sderive-metric" {
		t.Errorf(`RecordRefresh scheme = %q, want "sderive-metric" (the source field's own scheme)`, schemes[0])
	}
}

// TestDerivedFieldChangeSkipsDebugFieldUpdatedLog is the log-side counterpart
// to TestDerivedFieldChangeSkipsRefreshMetric: the per-field "field updated"
// debug log printed logAttrVersion for every FieldChange, including a derived
// one, which carries no Version at all - so it logged a misleading
// version="" for DSN. isDerivedPath skips it there too.
func TestDerivedFieldChangeSkipsDebugFieldUpdatedLog(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-log")
	wp.set("user", "alice", "v1")
	h, logger := newRecorder()

	type Config struct {
		User string `source:"sderive-log://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	sawUser := false
	for _, r := range h.all() {
		if r.Message != "field updated" {
			continue
		}
		field, _ := attrOf(r, logAttrField)
		if field == "DSN" {
			t.Errorf(`a debug "field updated" record was logged for the derived field DSN, want it skipped: %+v`, r)
		}
		if field == "User" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Error(`no "field updated" record found for User; the source field's own log line must still fire`)
	}
}

// TestDeriveDeclaredWriteSharingSourceTagStillMetersLogsAndDeduplicates is the
// regression test for the whole-branch review's I1 finding: isDerivedPath
// used to decide "is this FieldChange derived" by checking whether its path
// appeared in some WithDerive hook's declared writes, which stays true even
// when that same path ALSO carries its own source tag - a combination the
// design spec calls legal ("the derive simply wins") and one a caller
// produces by accident whenever they declare a derive's INPUT/output field
// rather than some other name. Probed directly, matching the review's own
// probe: DSN below carries both a source tag and a WithDerive write
// declaration, and rotating its provider value used to leave RecordRefresh
// uncalled, the "field updated" debug record unlogged, and DSN appearing
// twice in both Change.Fields and Status().Fields (once from the per-spec
// loop with a real Old/NewVersion or Ref/Scheme, once Derived-flavored with
// neither).
//
// isDerivedPath (reconciler.go) now decides structurally - a path with a
// fieldSpec is never derived, full stop, regardless of whether some hook also
// declares it as a write - and derivedFieldChanges/buildReport now refuse to
// produce the redundant second entry for such a path at all (hasSpecPath).
// This asserts every one of those properties in one probe: the metric, the
// log, and both report shapes.
func TestDeriveDeclaredWriteSharingSourceTagStillMetersLogsAndDeduplicates(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-collision")
	wp.set("dsn", "first", "v1")
	m := &recordingMeter{}
	h, logger := newRecorder()

	type Config struct {
		DSN string `source:"sderive-collision://dsn"`
	}
	events := make(chan Change[Config], 4)
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m), WithLogger(logger),
		// Declares DSN written without touching it: isolates the exact
		// accident the finding describes (a write path that also names a
		// source-tagged field) from anything the hook's body might do.
		WithDerive(func(c *Config) error { return nil }, "DSN"),
		OnChange(func(ev Change[Config]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("dsn", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	waitFlushed(t, w, 2)

	// The refresh metric must still fire, under the real scheme, exactly once
	// - not skipped, and not fabricated under an empty scheme.
	schemes := m.refreshSchemeList()
	if len(schemes) != 1 || schemes[0] != "sderive-collision" {
		t.Fatalf(`RecordRefresh calls = %v, want exactly ["sderive-collision"]: a ref that genuinely rotated must still be metered even though its path is also declared as a WithDerive write`, schemes)
	}

	// The per-field debug log must still fire, exactly once.
	dsnLogCount := 0
	for _, r := range h.all() {
		if r.Message != "field updated" {
			continue
		}
		if field, _ := attrOf(r, logAttrField); field == "DSN" {
			dsnLogCount++
		}
	}
	if dsnLogCount != 1 {
		t.Errorf(`"field updated" logged %d times for DSN, want exactly 1`, dsnLogCount)
	}

	// Change.Fields must carry DSN exactly once, with the real version diff a
	// genuinely rotated ref carries - not the empty-version shape a derived
	// entry carries.
	select {
	case ev := <-events:
		count := 0
		for _, f := range ev.Fields {
			if f.Path != "DSN" {
				continue
			}
			count++
			if f.OldVersion != "v1" || f.NewVersion != "v2" {
				t.Errorf("Change.Fields DSN entry = %+v, want OldVersion=v1 NewVersion=v2", f)
			}
		}
		if count != 1 {
			t.Fatalf("Change.Fields carries DSN %d times, want exactly 1: %+v", count, ev.Fields)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for the rotation")
	}

	// Status().Fields must carry DSN exactly once too, as an ordinary sourced
	// field (Derived: false, a real Scheme/Ref), not a duplicated Derived row.
	rep := w.Status()
	statusCount := 0
	for _, f := range rep.Fields {
		if f.Path != "DSN" {
			continue
		}
		statusCount++
		if f.Derived {
			t.Errorf("Status().Fields DSN entry has Derived = true, want false: DSN has its own fieldSpec, which must win")
		}
		if f.Scheme != "sderive-collision" {
			t.Errorf("Status().Fields DSN entry has Scheme = %q, want %q", f.Scheme, "sderive-collision")
		}
	}
	if statusCount != 1 {
		t.Fatalf("Status().Fields carries DSN %d times, want exactly 1: %+v", statusCount, rep.Fields)
	}
}

// TestDerivedFieldChangeSkipsRefreshMetricWhilePinned is the pinned-branch
// counterpart to TestDerivedFieldChangeSkipsRefreshMetric above. flush's
// pinned branch (the `if e.pinned` case, reconciler.go) carries its own
// isDerivedPath-gated RecordRefresh loop, a second copy of the same skip the
// unpinned branch performs a few lines below it, and nothing exercised it
// directly: deleting that guard leaves the whole suite green, because no
// other pinned test in this package (pin_test.go) installs a WithDerive hook
// at all. Pinning does not stop RecordRefresh from firing for the source
// field that actually rotated - flush's own doc comment is explicit that the
// counter documents "a watched value changed and was reconciled" regardless
// of pin state - so the derived path must still be skipped there for the
// identical reason the unpinned branch skips it.
func TestDerivedFieldChangeSkipsRefreshMetricWhilePinned(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-pinned-metric")
	wp.set("user", "alice", "v1")
	m := &recordingMeter{}

	type Config struct {
		User string `source:"sderive-pinned-metric://user"`
		DSN  string
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.User; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.PinCurrent(); got != 1 {
		t.Fatalf("PinCurrent() = %d, want 1", got)
	}

	wp.push("user", "bob", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	// Live still advances while pinned (only Get/OnChange freeze), so this is
	// the same proof-of-completion waitFlushed uses everywhere else.
	waitFlushed(t, w, 2)

	schemes := m.refreshSchemeList()
	if len(schemes) != 1 {
		t.Fatalf("RecordRefresh called %d times for a pinned flush with one source field and one derived field, want exactly 1 (the derived rebuild must not record its own refresh even while pinned): %v", len(schemes), schemes)
	}
	if schemes[0] != "sderive-pinned-metric" {
		t.Errorf(`RecordRefresh scheme = %q, want "sderive-pinned-metric" (the source field's own scheme)`, schemes[0])
	}
}

// TestStatusReportsSensitiveOnDerivedSecretField is the regression test for a
// review finding (M3): a derived FieldStatus's Sensitive was always false,
// even when the declared write path resolves to a secret.String or
// secret.Bytes field - exactly the type derived-fields.md tells a caller to
// assign a derived DSN into, so the CLI's SENSITIVE column read false for
// precisely the field an operator scans it for. buildReport (report.go) now
// checks the resolved field's own reflect.Type, the identical
// secretStringType/secretBytesType comparison walkSpecs (decode.go) already
// uses for a sourced field.
func TestStatusReportsSensitiveOnDerivedSecretField(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("sderive-sensitive")
	wp.set("user", "alice", "v1")

	type Config struct {
		User string `source:"sderive-sensitive://user"`
		DSN  secret.String
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk),
		WithDerive(func(c *Config) error {
			c.DSN = secret.NewString("postgres://" + c.User)
			return nil
		}, "DSN"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("Status().Fields has no entry for DSN: %+v", rep.Fields)
	}
	if !found.Sensitive {
		t.Error("DSN entry has Sensitive = false, want true: DSN resolves to a secret.String")
	}
	if found.Ref != "" || found.Scheme != "" {
		t.Errorf("DSN entry carries Ref=%q Scheme=%q, want both empty: a derived field has no ref", found.Ref, found.Scheme)
	}
}

// TestFieldUnhealthyDerivedCountsInvalidHook replaces the short-circuit test
// this guard used to need. A derived row carrying KindInvalid from a failed
// Doctor hook must count as unhealthy; a healthy derived row must not.
func TestFieldUnhealthyDerivedCountsInvalidHook(t *testing.T) {
	if !fieldUnhealthy(FieldStatus{Path: "DSN", Derived: true, LastKind: KindInvalid}) {
		t.Fatal("a derived row with KindInvalid must be unhealthy")
	}
	if fieldUnhealthy(FieldStatus{Path: "DSN", Derived: true, Version: "abc"}) {
		t.Fatal("a healthy derived row must not be unhealthy")
	}
}

// fieldByPathOuter/fieldByPathInner are the fixture for TestFieldByPath, a
// direct table test of the path-walking helper itself (decode.go): every
// caller (derivedFieldChanges, buildReport) only ever exercises it indirectly
// through a full Watch, which is not enough to pin the multi-segment walk on
// its own - mutating fieldByPath to unconditionally reject any path
// containing "." left the whole suite green, because none of the derive
// fixtures elsewhere in this file declare a nested write path.
type fieldByPathOuter struct {
	Name  string
	Inner fieldByPathInner
}
type fieldByPathInner struct {
	DSN string
}

func TestFieldByPath(t *testing.T) {
	cfg := fieldByPathOuter{Name: "top", Inner: fieldByPathInner{DSN: "nested-value"}}
	root := reflect.ValueOf(cfg)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"top-level field", "Name", "top"},
		{"nested field (the brief's own Redis.DSN shape)", "Inner.DSN", "nested-value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := fieldByPath(root, tt.path)
			if !ok {
				t.Fatalf("fieldByPath(%q) ok = false, want true", tt.path)
			}
			if got := v.String(); got != tt.want {
				t.Errorf("fieldByPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}

	notFoundTests := []struct {
		name string
		path string
	}{
		{"unknown top-level field", "Nope"},
		{"unknown nested field", "Inner.Nope"},
		{"segment that is not itself a struct", "Name.Deep"},
		{"empty path", ""},
	}
	for _, tt := range notFoundTests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := fieldByPath(root, tt.path); ok {
				t.Errorf("fieldByPath(%q) ok = true, want false", tt.path)
			}
		})
	}
}

// TestWithDeriveCopiesWrites pins that WithDerive does not retain the caller's
// slice. A variadic call spelled WithDerive(fn, paths...) hands over its own
// backing array; storing it directly would let a later mutation change the
// declared write paths, and would race the reconciler goroutine reading them.
func TestWithDeriveCopiesWrites(t *testing.T) {
	paths := []string{"DSN"}
	o := defaultOptions()
	WithDerive(func(*struct{}) error { return nil }, paths...)(o)

	paths[0] = "MUTATED"

	if got := o.derives[0].writes[0]; got != "DSN" {
		t.Fatalf("stored write path = %q, want %q: WithDerive kept the caller's slice", got, "DSN")
	}
}
