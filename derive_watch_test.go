// This file (package mamori_test, not mamori) exercises WithDerive on the
// Watch path: every case in derive_test.go calls Load, so none of them ever
// exercised the reconciler's own candidate build, only the loadValue path
// Load and Watch's initial resolve share. It needs mamoritest, which imports
// mamori itself, so it cannot live in derive_test.go (package mamori) without
// an import cycle - see decodepath_test.go's own comment on the same
// constraint. Every other file in this package that uses mamoritest
// (pin_test.go, history_test.go, bench_test.go, example_test.go) is in
// mamori_test for the identical reason.
package mamori_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"github.com/xavidop/mamori/secret"
)

// rotCfg is the motivating shape for WithDerive: a DSN assembled from a host
// (User, here), and a rotating secret. Every test below shares it, so a
// rotation always looks the same: p.Set("pass", ...) followed by a wait for
// the next reconciled snapshot.
type rotCfg struct {
	User string        `source:"fake://user"`
	Pass secret.String `source:"fake://pass"`
	DSN  secret.String
}

// buildRotDSN is the derive every test in this file installs. Assigning into
// a secret.String, not a plain string, is what keeps the assembled value
// redacted in fmt, %+v, and JSON - see TestDeriveSecretHygiene.
func buildRotDSN(c *rotCfg) error {
	c.DSN = secret.NewString("postgres://" + c.User + ":" + c.Pass.Reveal() + "@db")
	return nil
}

// TestDeriveRebuildsOnRotation is the single most important test in the
// feature: a DSN assembled from a host, a user, and a rotating password must
// be rebuilt on every reconciled update, not merely on the initial Load. Task
// 2 made the first build correct; this is what proves the rebuild survives a
// rotation, which is the entire point of wiring derives into the reconciler
// path.
//
// The design doc for this feature (docs/superpowers/specs/2026-07-31-derived-
// fields-design.md, "Change detection comes free") claims ev.Changed("DSN")
// is true here "with no special handling... a consequence of where derive
// sits". That does not hold against this engine: buildCandidate's diff loop
// (reconciler.go) walks e.specs, which fieldSpecs (decode.go) populates only
// for fields carrying a `source` tag - the same set the design doc's own
// "What mamori cannot know" section says is invisible to Status, explain,
// schema, and diff, for the identical reason (no tag, so no spec). DSN has no
// source tag, so it is never in e.specs, is never in e.observed/e.applied,
// and buildCandidate's diff (and diffApplied's, at Unpin) can never emit a
// FieldChange for it - regardless of where in buildCandidate the derive loop
// runs. This is verified empirically below, not assumed: ev.Changed("Pass")
// is asserted true (the real underlying source field the rotation touched),
// and no code inspected anywhere in this package computes Fields by any
// mechanism other than that same spec-keyed loop. Flagged in the Task 3
// report as a design/implementation mismatch worth a follow-up decision
// (fix the doc, or teach buildCandidate to diff derived fields too); this
// test asserts what the shipped engine actually does.
//
// The Change event is captured on a buffered channel rather than a bare
// shared bool: OnChange is dispatched asynchronously on its own goroutine
// (engine.enqueue only queues the event; it does not wait for the dispatch
// goroutine to run the callback), so a plain `changed = ev.Changed(...)`
// followed by an unsynchronized read after WaitForSnapshot returns would be a
// data race under -race, and could observe the value before the callback
// runs even if it weren't. flush's enqueue call happens on the reconciler
// goroutine strictly before the report.Store WaitForSnapshot polls for, and
// the channel is buffered, so by the time WaitForSnapshot returns the event
// is already sitting in the channel, ready to receive with no further wait.
func TestDeriveRebuildsOnRotation(t *testing.T) {
	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	events := make(chan mamori.Change[rotCfg], 4)
	w, err := mamori.Watch[rotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(buildRotDSN),
		mamori.OnChange(func(ev mamori.Change[rotCfg]) { events <- ev }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().DSN.Reveal(); got != "postgres://alice:old@db" {
		t.Fatalf("initial: got %q", got)
	}

	p.Set("pass", "new")
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().DSN.Reveal(); got != "postgres://alice:new@db" {
		t.Fatalf("after rotation: got %q, want the rebuilt DSN", got)
	}

	select {
	case ev := <-events:
		if !ev.Changed("Pass") {
			t.Error(`ev.Changed("Pass") must be true after a rotation that changed the password`)
		}
		if got := ev.New.DSN.Reveal(); got != "postgres://alice:new@db" {
			t.Errorf("Change.New.DSN = %q, want the rebuilt DSN", got)
		}
	default:
		t.Error("OnChange did not fire for the rotation")
	}
}

// TestDeriveRunsBeforePreApplyOnWatch proves the second load-bearing ordering
// property: a PreApply gate must be handed the REBUILT DSN, not the one it is
// about to replace, because a rotation-safety hook's entire job is proving
// the new value works. If the reconciler ran derives after the PreApply gate
// (rather than in buildCandidate, before flush ever calls it), capturedDSN
// below would still read the pre-derive zero value after the rotation, never
// the rebuilt DSN - which is the point of this test.
func TestDeriveRunsBeforePreApplyOnWatch(t *testing.T) {
	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	var mu sync.Mutex
	var capturedDSN string
	w, err := mamori.Watch[rotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(buildRotDSN),
		mamori.PreApply(func(_ context.Context, ev mamori.Change[rotCfg]) error {
			mu.Lock()
			capturedDSN = ev.New.DSN.Reveal()
			mu.Unlock()
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("pass", "new")
	mamoritest.WaitForSnapshot(t, w, 2)

	mu.Lock()
	got := capturedDSN
	mu.Unlock()
	want := "postgres://alice:new@db"
	if got != want {
		t.Fatalf("PreApply saw DSN %q, want the rebuilt %q", got, want)
	}
}

// TestDeriveErrorRejectsUpdate: a derive error rejects the whole candidate
// atomically, exactly as a validation failure does. Get must keep serving the
// previous config, and OnError must receive a *DeriveError whose cause
// survives errors.Is.
func TestDeriveErrorRejectsUpdate(t *testing.T) {
	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	boom := errors.New("boom")
	var fail atomic.Bool
	buildDSN := func(c *rotCfg) error {
		if fail.Load() {
			return boom
		}
		c.DSN = secret.NewString("postgres://" + c.User + ":" + c.Pass.Reveal() + "@db")
		return nil
	}

	errOpt, errCap := mamoritest.CaptureErrors()
	w, err := mamori.Watch[rotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(buildDSN),
		errOpt,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().DSN.Reveal(); got != "postgres://alice:old@db" {
		t.Fatalf("initial: got %q", got)
	}

	fail.Store(true)
	p.Set("pass", "new")

	gotErr := mamoritest.WaitForError(t, errCap, mamori.KindUnknown)
	var de *mamori.DeriveError
	if !errors.As(gotErr, &de) {
		t.Fatalf("got %T, want *mamori.DeriveError", gotErr)
	}
	if !errors.Is(gotErr, boom) {
		t.Error("the cause must survive errors.Is")
	}
	if got := w.Get().DSN.Reveal(); got != "postgres://alice:old@db" {
		t.Fatalf("after a rejected derive: got %q, want the previous config preserved", got)
	}
}

// validatedRotCfg pairs a derived, required field with a source-tagged input,
// so a reconciled candidate's validation outcome depends entirely on whether
// the derive already ran: DSN carries no source tag of its own (nothing else
// can fill it), so a candidate that reached validation without the derive
// having run first would always fail it.
type validatedRotCfg struct {
	User string `source:"fake-validated://user"`
	DSN  string `validate:"required"`
}

// TestDeriveRunsBeforeValidationOnWatch is buildCandidate's own counterpart to
// derive_test.go's TestDeriveRunsBeforeValidation: that test pins the
// ordering for loadValue, which Load and Watch's initial resolve share, but
// buildCandidate (reconciler.go) is a second, separate place a candidate is
// built and validated - on every RECONCILED update after the first - and
// nothing in Task 2 ever exercised it. If the derive loop in buildCandidate
// were moved to after e.o.validator.Validate(cand), this reconcile would
// validate a candidate whose DSN is still the zero value (DSN has no source
// tag, so only the derive ever fills it), rejecting a candidate that should
// have passed: the snapshot version would never advance past 1, and
// WaitForSnapshot below would time out and fail the test - a rejected
// candidate never advances e.version (see flush), so reaching version 2 at
// all is already proof the reconciled candidate passed validation, and the
// DSN check confirms it passed WITH the re-derived value rather than some
// stale one.
func TestDeriveRunsBeforeValidationOnWatch(t *testing.T) {
	p := mamoritest.NewProvider("fake-validated")
	p.Set("user", "alice")

	fillDSN := func(c *validatedRotCfg) error {
		c.DSN = "postgres://" + c.User
		return nil
	}

	w, err := mamori.Watch[validatedRotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(fillDSN),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().DSN; got != "postgres://alice" {
		t.Fatalf("initial: got %q", got)
	}

	p.Set("user", "bob")
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().DSN; got != "postgres://bob" {
		t.Fatalf("after reconcile: got %q, want the re-derived DSN", got)
	}
}

// TestDeriveTypeMismatchFailsWatch: a derive whose type parameter does not
// match Watch's must fail Watch loudly, satisfying ErrInvalid, rather than
// being silently skipped - the property that distinguishes WithDerive from
// OnChange's silent discard (bug #111). User/Pass are populated so the
// provider round trip that resolveAll performs can succeed regardless of
// exactly where the mismatch is caught (loadValue's own check, or an earlier
// one Watch performs before calling loadValue at all): either placement must
// fail Watch the same way.
func TestDeriveTypeMismatchFailsWatch(t *testing.T) {
	type otherCfg struct{ Z string }

	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	_, err := mamori.Watch[rotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(func(c *otherCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a mismatched derive type must fail Watch, not be silently skipped")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("got %v, want an error satisfying ErrInvalid", err)
	}
}

// TestDeriveSecretHygiene guards the property that makes WithDerive safe for
// assembling a DSN: assigning into a secret.String, not a plain string, is
// what keeps the derived value redacted everywhere it might leak - fmt's %v
// and %+v, and JSON. This is the test that would catch a future doc example
// or snippet steering readers toward a plain string target, which would leak
// the rotated password into logs, %+v dumps, and JSON payloads alike.
func TestDeriveSecretHygiene(t *testing.T) {
	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	w, err := mamori.Watch[rotCfg](context.Background(),
		mamori.WithProvider(p),
		mamori.WithDerive(buildRotDSN),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("pass", "new")
	mamoritest.WaitForSnapshot(t, w, 2)

	cfg := w.Get()
	if got := cfg.DSN.Reveal(); got != "postgres://alice:new@db" {
		t.Fatalf("got %q, want the rebuilt DSN", got)
	}

	leaked := []string{"new", "postgres://alice:new@db"}

	if s := fmt.Sprintf("%v", cfg.DSN); leaksAny(s, leaked) {
		t.Errorf("fmt %%v of the derived secret.String leaked the rotated secret: %s", s)
	}
	if s := fmt.Sprintf("%+v", cfg); leaksAny(s, leaked) {
		t.Errorf("fmt %%+v of the config leaked the rotated secret: %s", s)
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if leaksAny(string(b), leaked) {
		t.Errorf("json.Marshal leaked the rotated secret: %s", b)
	}
}

func leaksAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
