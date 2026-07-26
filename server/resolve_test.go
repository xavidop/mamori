package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// resolveTestWait bounds how long the polling helper below waits for a
// binding's snapshot to reach an expected state. The fan-out path under test
// here is entirely in-memory (mamoritest.Provider's Watch delivers
// synchronously into a buffered channel - see mamoritest/mamoritest.go), so
// anything not landing within this window is a real bug, not a slow backend;
// it mirrors mamoritest.WaitForSnapshot's own defaultWait (mamoritest/wait.go).
const resolveTestWait = 2 * time.Second

// countingProvider wraps a mamoritest.Provider and counts calls to Watch, so
// a test can prove that N concurrent lookups against one binding produced
// exactly one upstream watch - the fan-out property this file exists to
// implement - rather than one watch per reader. Scheme and Resolve are
// promoted unchanged from the embedded *mamoritest.Provider; only Watch is
// intercepted.
type countingProvider struct {
	*mamoritest.Provider
	watches atomic.Int64
}

func newCountingProvider(scheme string) *countingProvider {
	return &countingProvider{Provider: mamoritest.NewProvider(scheme)}
}

func (c *countingProvider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	c.watches.Add(1)
	return c.Provider.Watch(ctx, ref)
}

// emptyValueProvider is a minimal, hand-rolled mamori.WatchableProvider used
// by exactly one test (TestStaleEmptyLastKnownGoodValueIsServedNotFailed)
// instead of mamoritest.Provider, for one reason: mamoritest.Provider.Set/
// SetBytes always derives Version via mamori.VersionHash (see valueOf in
// mamoritest/mamoritest.go), so even an explicitly empty Set("k", "") comes
// back with a non-empty hash Version. That is enough for even the OLD, buggy
// shape-guess (len(Bytes)!=0 || Version!="") to (by accident, not because
// the guess was sound) still classify the value as resolved. Proving the
// fix requires a value that is genuinely empty on BOTH axes at once - Bytes:
// []byte{}, Version: "" - which mamori.Value's own doc comment (value.go)
// confirms is an ordinary, supported shape ("Version ... If empty, mamori
// falls back to byte comparison"), not a contrived one: plenty of real
// providers never set a Version at all. emptyValueProvider drives exactly
// that update, once told to via succeedEmpty/fail below.
type emptyValueProvider struct {
	scheme string
	ch     chan mamori.Update
}

func newEmptyValueProvider(scheme string) *emptyValueProvider {
	return &emptyValueProvider{scheme: scheme, ch: make(chan mamori.Update, 4)}
}

// Scheme implements mamori.Provider.
func (p *emptyValueProvider) Scheme() string { return p.scheme }

// Resolve implements mamori.Provider so emptyValueProvider satisfies the
// interface; start (resolve.go) only ever reaches WatchableProvider's Watch
// below for a provider that implements it, so this is never actually called
// by this file's tests.
func (p *emptyValueProvider) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	return mamori.Value{}, mamori.ErrNotFound
}

// Watch implements mamori.WatchableProvider by handing back the provider's
// one channel: resolve.go's start reads from it once via mamori.WatchRef,
// and whatever succeedEmpty/fail push onto it below is what that binding's
// watch goroutine applies.
func (p *emptyValueProvider) Watch(context.Context, mamori.Ref) (<-chan mamori.Update, error) {
	return p.ch, nil
}

// succeedEmpty pushes a successful Update carrying the genuinely empty
// value this test exists to exercise: zero-length Bytes and an empty
// Version, a legitimate (if unusual) last-known-good value.
func (p *emptyValueProvider) succeedEmpty() {
	p.ch <- mamori.Update{Value: mamori.Value{Bytes: []byte{}}}
}

// fail pushes an upstream failure Update, simulating this binding's
// provider going from its one successful (empty) resolve to erroring.
func (p *emptyValueProvider) fail(err error) {
	p.ch <- mamori.Update{Err: err}
}

// waitForLookup polls s.lookup(name) until want reports true, returning the
// state it observed. It fails the test if that does not happen within
// resolveTestWait. Like mamoritest.WaitForSnapshot, this exists so a test
// that pushes a change or failure through the provider can block
// deterministically on the server's own watch goroutine having applied it,
// rather than sleeping a fixed, either-flaky-or-slow duration.
func waitForLookup(t *testing.T, s *Server, name string, want func(value mamori.Value, kind mamori.Kind, hasValue, found bool) bool) (mamori.Value, mamori.Kind, bool, bool) {
	t.Helper()
	deadline := time.Now().Add(resolveTestWait)
	var v mamori.Value
	var k mamori.Kind
	var hasValue, found bool
	for time.Now().Before(deadline) {
		v, k, hasValue, found = s.lookup(name)
		if want(v, k, hasValue, found) {
			return v, k, hasValue, found
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mamori/server: lookup(%q) did not reach expected state within %s (last observed: value=%q kind=%q hasValue=%v found=%v)",
		name, resolveTestWait, v.Bytes, k, hasValue, found)
	return v, k, hasValue, found
}

// TestFanOutOneUpstreamWatchServesManyReads is the fan-out proof: 100
// concurrent lookups of the same binding must be served by exactly one
// mamori.WatchRef/Watch call against the upstream provider, not one per
// reader.
func TestFanOutOneUpstreamWatchServesManyReads(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := newCountingProvider("fanout")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "fanout://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	const readers = 100
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, k, _, found := s.lookup("b")
			if !found {
				t.Errorf("lookup(b) found = false, want true")
			}
			if k != "" {
				t.Errorf("lookup(b) kind = %q, want empty", k)
			}
			if string(v.Bytes) != "v1" {
				t.Errorf("lookup(b) value = %q, want %q", v.Bytes, "v1")
			}
		}()
	}
	wg.Wait()

	if got := p.watches.Load(); got != 1 {
		t.Fatalf("provider.Watch called %d times, want exactly 1 (one upstream watch for %d concurrent readers)", got, readers)
	}
}

// TestSetPropagatesToSubsequentReads confirms a change pushed through the
// upstream provider after start reaches later lookup calls, proving the
// binding's watch goroutine keeps its snapshot current rather than only
// capturing the value present at start time.
func TestSetPropagatesToSubsequentReads(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("setprop")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "setprop://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	p.Set("k", "v2")

	v, k, _, found := waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v2"
	})
	if !found || k != "" || string(v.Bytes) != "v2" {
		t.Fatalf("after Set(v2): value=%q kind=%q found=%v, want v2/empty/true", v.Bytes, k, found)
	}
}

// TestFailServesLastKnownGoodWithErrorKind confirms that once a binding has
// resolved successfully, an upstream failure does not blank it out: lookup
// keeps returning the last-known-good value while kind reports the upstream
// error's classification, so a caller can tell fresh from stale-but-serving.
func TestFailServesLastKnownGoodWithErrorKind(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("failgood")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "failgood://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	injected := fmt.Errorf("backend down: %w", mamori.ErrUnavailable)
	p.Fail("k", injected)

	v, k, hasValue, found := waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == mamori.KindUnavailable
	})
	if !found {
		t.Fatal("found = false, want true")
	}
	if !hasValue {
		t.Fatal("hasValue = false, want true (a last-known-good value exists)")
	}
	if string(v.Bytes) != "v1" {
		t.Fatalf("stale value = %q, want last-known-good %q", v.Bytes, "v1")
	}
	if k != mamori.KindUnavailable {
		t.Fatalf("kind = %q, want %q", k, mamori.KindUnavailable)
	}
}

// TestNeverResolvedBindingReturnsUpstreamErrorKind covers the other half of
// the last-good contract: a binding that has never once resolved
// successfully has no last-known-good value to fall back to, so lookup must
// report the upstream error/kind with a zero Value, not a misleading empty
// success.
func TestNeverResolvedBindingReturnsUpstreamErrorKind(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("neverresolved")
	injected := fmt.Errorf("access denied: %w", mamori.ErrPermissionDenied)
	// Fail, never Set, and BEFORE start: mamoritest.Provider.Watch replays
	// the injected failure as the very first (baseline) update, so this
	// binding never has a value to remember in the first place.
	p.Fail("missing", injected)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "neverresolved://missing"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	v, k, hasValue, found := waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == mamori.KindPermissionDenied
	})
	if !found {
		t.Fatal("found = false, want true")
	}
	if hasValue {
		t.Fatal("hasValue = true, want false (never resolved)")
	}
	if k != mamori.KindPermissionDenied {
		t.Fatalf("kind = %q, want %q", k, mamori.KindPermissionDenied)
	}
	if len(v.Bytes) != 0 || v.Version != "" {
		t.Fatalf("value = %+v, want the zero Value (never resolved)", v)
	}
}

// TestStaleEmptyLastKnownGoodValueIsServedNotFailed guards the fix in this
// file: lookup's hasValue return is the authoritative
// bindingSnapshot.hasValue, not a guess from the value's shape. A binding
// whose one and only successful resolve produced a genuinely empty value
// (Bytes: []byte{}, Version: "" - see emptyValueProvider's doc comment for
// why mamoritest.Provider cannot produce this shape) must still be served
// STALE - hasValue true, the value, and the upstream's error kind - once its
// provider starts failing, exactly like TestFailServesLastKnownGoodWithErrorKind's
// non-empty case above. Before this fix, the handler layer inferred
// "resolved" from `len(v.Bytes) != 0 || v.Version != ""` (wire.go's since-
// deleted hasResolvedValue): both operands are false for this value, so the
// old guess misread a stale-but-legitimate empty value as "never resolved"
// and would have reported a hard failure instead of serving it.
func TestStaleEmptyLastKnownGoodValueIsServedNotFailed(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := newEmptyValueProvider("emptygood")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "emptygood://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	p.succeedEmpty()

	v, k, hasValue, found := waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue && k == ""
	})
	if !found || !hasValue || k != "" {
		t.Fatalf("after the empty resolve: value=%+v kind=%q hasValue=%v found=%v, want hasValue=true kind=empty found=true", v, k, hasValue, found)
	}
	// Confirm the fixture is actually the shape this test needs before
	// relying on it below: genuinely zero-length Bytes AND an empty Version.
	if len(v.Bytes) != 0 || v.Version != "" {
		t.Fatalf("test fixture invalid: value = %+v is not the empty-bytes/empty-version shape this test requires", v)
	}

	injected := fmt.Errorf("backend down: %w", mamori.ErrUnavailable)
	p.fail(injected)

	v, k, hasValue, found = waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == mamori.KindUnavailable
	})
	if !found {
		t.Fatal("found = false, want true")
	}
	if !hasValue {
		t.Fatal("hasValue = false, want true: a last-known-good value exists (it is just empty), so this must be served stale, not reported as a hard failure")
	}
	if k != mamori.KindUnavailable {
		t.Fatalf("kind = %q, want %q", k, mamori.KindUnavailable)
	}
	if len(v.Bytes) != 0 || v.Version != "" {
		t.Fatalf("stale value = %+v, want the still-empty last-known-good value", v)
	}

	// Prove this fixture actually reproduces the bug, not just exercises the
	// fix against an input the bug never touched. oldGuess recomputes the
	// exact formula the now-deleted wire.go hasResolvedValue used to apply
	// (it cannot be called directly any more - that is the point of the
	// fix): len(v.Bytes) != 0 || v.Version != "". The OLD handler branched
	// on `k != "" && !hasResolvedValue(v)`, i.e. `k != "" && !oldGuess`; for
	// this exact (kind, value) pair that was true, so the old code would
	// have reported a hard resolve failure (statusForKind(k), no value)
	// instead of serving v stale. hasValue (asserted true above, and now
	// what the handler actually branches on) disagrees with oldGuess here -
	// that disagreement is exactly the bug this test guards against.
	oldGuess := len(v.Bytes) != 0 || v.Version != ""
	if oldGuess {
		t.Fatal("test fixture does not reproduce the bug: the old shape-guess would have (correctly, by coincidence) classified this value as resolved, so this test would not catch a regression back to it")
	}
	if hasValue == oldGuess {
		t.Fatalf("hasValue (%v) and the old shape-guess (%v) agree for this fixture, want them to disagree - that disagreement is what this test exists to prove", hasValue, oldGuess)
	}
	oldHandlerWouldHardFail := k != "" && !oldGuess
	if !oldHandlerWouldHardFail {
		t.Fatal("test fixture does not reproduce the bug: the old handler branch condition would not have treated this as a hard failure")
	}
}

// TestLookupUnknownBindingNotFound confirms lookup distinguishes "not a
// bound name at all" (found=false) from every other outcome, which is what
// lets the wire protocol handler (a later task) answer a request for an
// unbound name without touching Policy or leaking whether the name exists.
func TestLookupUnknownBindingNotFound(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("unknownbind")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "unknownbind://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, _, _, found := s.lookup("does-not-exist"); found {
		t.Fatal("lookup of an unbound name returned found = true")
	}
}

// TestMixedCaseSchemeGateAndLookupAgreeOnCanonicalScheme is the end-to-end
// proof that bindings.go's resolveBindings and resolve.go's start/lookup now
// agree on one canonical (lowercased) scheme for the same Binding, closing
// the gap where the exec:/mamori: gate compared a lowercased ref.Scheme
// locally while resolve.go's s.providers[b.Ref.Scheme] lookup compared the
// stored (possibly mixed-case) scheme verbatim. Before the fix, a binding
// like Bind("x", "EXEC:...") with AllowExec() passed New (the gate matched
// "exec" case-insensitively) but then failed at start with errNoProvider,
// because s.providers only ever holds lowercase keys (WithProvider stores
// under Provider.Scheme(), which real providers - and mamoritest.Provider
// here - report in lowercase) and "EXEC" != "exec" under a plain map lookup.
//
// This binds under the mixed-case scheme "EXEC:", gated by AllowExec(), with
// a mamoritest.Provider registered for the lowercase scheme "exec" (the
// closest in-repo equivalent to core's exec: provider - see AllowExec's doc
// comment in server.go for why core's real exec: provider can't be wired up
// here directly). If the gate and the lookup still disagreed on the
// canonical scheme, this binding would resolve to errNoProvider forever, the
// exact bug reported in the finding this test guards against. With the fix,
// resolveBindings stores ref.Scheme already lowercased, so the Binding
// start/lookup see carries "exec", matching the provider's key exactly, and
// the binding resolves normally.
func TestMixedCaseSchemeGateAndLookupAgreeOnCanonicalScheme(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("exec")
	p.Set("echo hi", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("x", "EXEC:echo hi"),
		AllowExec(),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The stored binding's scheme must already be canonical: both the gate
	// (bindings.go) and provider lookup (resolve.go) read this same field, so
	// if either still saw "EXEC" instead of "exec" the two would disagree
	// again, exactly the inconsistency this test exists to catch.
	if got := s.bindings["x"].Ref.Scheme; got != "exec" {
		t.Fatalf("stored binding scheme = %q, want canonical %q", got, "exec")
	}
	// ref.Raw must still show the operator's original tag verbatim, for
	// diagnostics - only the Scheme field used for gating+lookup is
	// canonicalized.
	if got := s.bindings["x"].Ref.Raw; got != "EXEC:echo hi" {
		t.Fatalf("stored binding Ref.Raw = %q, want original tag %q", got, "EXEC:echo hi")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	v, k, _, found := waitForLookup(t, s, "x", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == ""
	})
	if !found {
		t.Fatal("found = false, want true")
	}
	if k != "" {
		t.Fatalf("kind = %q, want empty (resolved through the \"exec\"-registered provider)", k)
	}
	if string(v.Bytes) != "v1" {
		t.Fatalf("value = %q, want %q", v.Bytes, "v1")
	}
}

// TestStartWithNoProviderRegisteredRecordsError confirms a binding whose
// scheme has no WithProvider registration does not fail the whole server at
// start: it gets a clear, classified resolution error of its own (and no
// watch goroutine, since there is nothing to watch), rather than start
// erroring out or panicking.
func TestStartWithNoProviderRegisteredRecordsError(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "unregistered://k"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	v, k, hasValue, found := s.lookup("b")
	if !found {
		t.Fatal("found = false, want true")
	}
	if hasValue {
		t.Fatal("hasValue = true, want false (no provider registered, so never resolved)")
	}
	if k != mamori.KindUnknown {
		t.Fatalf("kind = %q, want %q (no provider registered for scheme)", k, mamori.KindUnknown)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("value = %+v, want the zero Value", v)
	}
}

// TestStartCalledTwiceErrors confirms start refuses a second call rather
// than launching a second set of watch goroutines racing the first to
// publish into the same bindings.
func TestStartCalledTwiceErrors(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("starttwice")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "starttwice://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.start(ctx); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("second start error = %v, want errAlreadyStarted", err)
	}
}

// TestCloseWithoutStartIsSafe confirms Close is a safe no-op when start was
// never called, so a caller can unconditionally `defer s.Close()` right
// after New without knowing whether Serve/start ever actually ran.
func TestCloseWithoutStartIsSafe(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, err := New(WithPolicy(AllowAll()), NoAuth())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCloseIsIdempotent confirms a second Close (a deferred Close plus an
// explicit one on an error path, say) does not double-cancel or double-wait.
func TestCloseIsIdempotent(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("closetwice")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "closetwice://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestConcurrentReadsDuringUpstreamUpdates hammers lookup from many
// goroutines while another goroutine keeps pushing Set/Fail through the
// provider, so that `go test -race` can catch a torn read between a
// binding's value and its error/kind (see bindingSnapshot's doc comment in
// resolve.go for why they are bundled into one atomically-swapped struct
// rather than tracked as separate atomics).
func TestConcurrentReadsDuringUpstreamUpdates(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("racey")
	p.Set("k", "v0")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "racey://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == ""
	})

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			if i%2 == 0 {
				p.Set("k", fmt.Sprintf("v%d", i))
			} else {
				p.Fail("k", mamori.ErrUnavailable)
			}
		}
	}()

	const readers = 50
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				// Every observed pairing must be internally consistent: a
				// non-empty kind may accompany either a stale last-good value
				// or a zero one (never resolved), but never a torn mix of an
				// unrelated update's fields.
				v, k, hasValue, found := s.lookup("b")
				if !found {
					t.Errorf("lookup(b) found = false, want true")
				}
				// "b" resolved successfully (p.Set("k", "v0")) before this race
				// began, and bindingSnapshot.hasValue only ever goes false->true,
				// never back - so every lookup from here on must report hasValue
				// true, no matter how apply and lookup interleave concurrently.
				if !hasValue {
					t.Errorf("lookup(b) hasValue = false, want true (binding resolved before the race began)")
				}
				if k == "" && len(v.Bytes) == 0 {
					t.Errorf("lookup(b) = %+v, kind empty but no value either", v)
				}
			}
		}()
	}

	wg.Wait()
}
