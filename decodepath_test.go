package mamori

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// This file covers Task 6: ?decode= must be applied at every point a Value
// enters the engine, not just the obvious one. Each test below pins one of
// those entry points, so wiring any single site while missing another leaves a
// red test rather than a silent production bug.
//
// The tests live in package mamori (not mamori_test) for two reasons: the
// chain-seeding entry point is only reachable through the unexported
// engine.seedChainSources, and the watchProvider fake from watch_test.go can
// seed a value silently (set) and push an update separately (push), which is
// what makes "decoded on load but not on update" observable at all. A
// mamoritest.Provider always publishes a baseline on subscribe, so a missing
// decode on the update path would be masked by the baseline replay.

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestDecodeAppliesOnLoad covers resolveRef (resolve.go), the entry point used
// by Load, Doctor, and the reconciler's own re-resolves.
func TestDecodeAppliesOnLoad(t *testing.T) {
	type cfg struct {
		A string `source:"decl://a?decode=base64"`
	}
	p := newWatchProvider("decl")
	p.set("a", b64("plain"), "v1")

	got, err := Load[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.A != "plain" {
		t.Errorf("A = %q, want plain", got.A)
	}
}

// TestDecodeAppliesOnWatchUpdate is the regression test for the failure mode
// this task exists to prevent: decoding correctly on the initial load and then
// silently passing raw bytes through on every subsequent update. It covers
// recordSourceUpdate (reconciler.go), the single funnel every native watch and
// every poll update flows through.
func TestDecodeAppliesOnWatchUpdate(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"decw://a?decode=base64"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("decw")
	p.set("a", b64("first"), "v1")

	changed := make(chan cfg, 4)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		OnChange(func(ev Change[cfg]) { changed <- ev.New }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().A; got != "first" {
		t.Fatalf("initial A = %q, want first", got)
	}

	p.push("a", b64("second"), "v2")
	waitPending(clk)
	clk.Advance(defaultDebounce)

	select {
	case got := <-changed:
		if got.A != "second" {
			t.Errorf("after update A = %q, want second (decode was not applied on the watch path)", got.A)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the update")
	}
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second", got)
	}
}

// TestDecodeFailureOnWatchKeepsLastGoodValue pins the failure contract on the
// watch path: an undecodable update is a terminal, non-not-found error for
// that chain position, so the field's onfail policy applies (keeplast, the
// default) and the last good value survives. It must never panic and must
// never substitute the raw or empty bytes.
func TestDecodeFailureOnWatchKeepsLastGoodValue(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"decf://a?decode=base64"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("decf")
	p.set("a", b64("good"), "v1")

	changed := make(chan cfg, 4)
	errs := make(chan error, 4)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		OnChange(func(ev Change[cfg]) { changed <- ev.New }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "!!! not base64 !!!", "v2")
	waitPending(clk)
	clk.Advance(defaultDebounce)

	select {
	case e := <-errs:
		if !errors.Is(e, ErrInvalid) {
			t.Errorf("error = %v, want one wrapping ErrInvalid", e)
		}
		if ErrorKind(e) != KindInvalid {
			t.Errorf("ErrorKind = %q, want %q", ErrorKind(e), KindInvalid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error delivered for an undecodable watch update")
	}
	select {
	case ev := <-changed:
		t.Fatalf("OnChange fired for an undecodable update: %+v", ev)
	default:
	}
	if got := w.Get().A; got != "good" {
		t.Errorf("Get().A = %q, want good (last good value must survive a decode failure)", got)
	}
}

// batchDecodeProvider is a BatchProvider whose single-ref Resolve deliberately
// fails, so a test asserting a decoded result through it can only pass if
// resolveAll actually took the ResolveBatch path. There is no BatchProvider
// fake anywhere else in this package's tests (grep: the only in-repo batch
// tests are per-provider, in providers/mamori/batch_test.go), so this is the
// minimal one this entry point needs.
type batchDecodeProvider struct {
	scheme string
	data   map[string]string // keyed by Ref.Path
}

func (b *batchDecodeProvider) Scheme() string { return b.scheme }

func (b *batchDecodeProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	return Value{}, fmt.Errorf("batchDecodeProvider.Resolve called for %s: resolveAll must use ResolveBatch here", ref.Raw)
}

// ResolveBatch keys its result map by each input Ref's Raw, which is the
// contract resolveBatchScheme looks values up by (provider.go).
func (b *batchDecodeProvider) ResolveBatch(_ context.Context, refs []Ref) (map[string]Value, error) {
	out := make(map[string]Value, len(refs))
	for _, ref := range refs {
		v, ok := b.data[ref.Path]
		if !ok {
			continue // absent keys are simply omitted; resolveBatchScheme applies default/optional
		}
		out[ref.Raw] = Value{Bytes: []byte(v), Version: "v1"}
	}
	return out, nil
}

// TestDecodeAppliesOnBatchResolve covers resolveBatchScheme (resolve.go), the
// entry point every BatchProvider's values arrive through. A single-ref field
// of a batch-capable scheme never goes near resolveRef, so this site has to be
// wired independently of it.
func TestDecodeAppliesOnBatchResolve(t *testing.T) {
	type cfg struct {
		A string `source:"decb://a?decode=base64"`
		B string `source:"decb://b?decode=hex"`
		C string `source:"decb://c"`
	}
	p := &batchDecodeProvider{
		scheme: "decb",
		data: map[string]string{
			"a": b64("alpha"),
			"b": hex.EncodeToString([]byte("bravo")),
			"c": "charlie",
		},
	}

	got, err := Load[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.A != "alpha" {
		t.Errorf("A = %q, want alpha (decode was not applied on the batch path)", got.A)
	}
	if got.B != "bravo" {
		t.Errorf("B = %q, want bravo (decode was not applied on the batch path)", got.B)
	}
	if got.C != "charlie" {
		t.Errorf("C = %q, want charlie (a ref with no decode option must pass through untouched)", got.C)
	}
}

// TestDecodeFailureOnBatchResolveFailsLoad pins that an undecodable batch
// result fails the Load loudly rather than reaching the struct raw.
func TestDecodeFailureOnBatchResolveFailsLoad(t *testing.T) {
	type cfg struct {
		A string `source:"decbf://a?decode=base64"`
	}
	p := &batchDecodeProvider{scheme: "decbf", data: map[string]string{"a": "!!! not base64 !!!"}}

	_, err := Load[cfg](context.Background(), WithProvider(p))
	if err == nil {
		t.Fatal("Load succeeded; an undecodable batch value must fail the Load")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want one wrapping ErrInvalid", err)
	}
}

// TestDecodeAppliesOnChainSeed covers seedChainSources (reconciler.go), the
// fourth place a Value becomes engine state: the synchronous per-position walk
// engine.start performs for a multi-ref chain before any watch source exists.
// It calls Provider.Resolve directly rather than going through resolveRef, so
// it does not inherit resolveRef's decoding.
//
// Left unwired, a rotation landing between Watch's initial Load and this seed
// would store the RAW bytes at the winning position with the NEW version. The
// winner recompute triggered by any other position's first update would then
// publish those raw bytes, and the winning position's own watch baseline -
// carrying that same new version, correctly decoded - would be discarded as
// "unchanged" by markChanged's version comparison. The field would stay
// corrupted for the rest of the process's life.
func TestDecodeAppliesOnChainSeed(t *testing.T) {
	type cfg struct {
		A string `source:"decs1://x?decode=base64,decs2://y?decode=base64"`
	}
	a := newWatchProvider("decs1")
	b := newWatchProvider("decs2")
	a.set("x", b64("from-a"), "v1")
	b.set("y", b64("from-b"), "v1")

	o := defaultOptions()
	WithProvider(a)(o)
	WithProvider(b)(o)
	specs, err := fieldSpecs(reflect.TypeOf(cfg{}))
	if err != nil {
		t.Fatalf("fieldSpecs: %v", err)
	}
	e := &engine[cfg]{o: o}

	st := e.seedChainSources(context.Background(), specs[0])
	if st[0].err != nil {
		t.Fatalf("seeded position 0 error: %v", st[0].err)
	}
	if string(st[0].value.Bytes) != "from-a" {
		t.Errorf("seeded position 0 = %q, want from-a (decode was not applied when seeding chain state)", st[0].value.Bytes)
	}
	if st[0].value.Version != "v1" {
		t.Errorf("seeded Version = %q, want v1 (decoding must not disturb the provider's revision)", st[0].value.Version)
	}
}

// TestDecodeFailureOnChainSeedStopsWalk pins that an undecodable leading
// position is seeded as a terminal error, exactly as any other non-not-found
// resolve failure is, so recomputeWinner stops the walk there instead of
// sliding down to a lower-precedence source (spec 10.3 case 3).
func TestDecodeFailureOnChainSeedStopsWalk(t *testing.T) {
	type cfg struct {
		A string `source:"decsf1://x?decode=base64,decsf2://y"`
	}
	a := newWatchProvider("decsf1")
	b := newWatchProvider("decsf2")
	a.set("x", "!!! not base64 !!!", "v1")
	b.set("y", "from-b", "v1")

	o := defaultOptions()
	WithProvider(a)(o)
	WithProvider(b)(o)
	specs, err := fieldSpecs(reflect.TypeOf(cfg{}))
	if err != nil {
		t.Fatalf("fieldSpecs: %v", err)
	}
	e := &engine[cfg]{o: o}

	st := e.seedChainSources(context.Background(), specs[0])
	if st[0].err == nil {
		t.Fatal("seeded position 0 has no error; an undecodable value must stop the chain walk")
	}
	if !errors.Is(st[0].err, ErrInvalid) {
		t.Errorf("seeded position 0 error = %v, want one wrapping ErrInvalid", st[0].err)
	}
	if st[1].seen {
		t.Error("position 1 was seeded; a non-not-found error at position 0 must stop the walk, not fail over")
	}
}
