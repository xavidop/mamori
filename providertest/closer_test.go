// This file is package providertest, not providertest_test like the rest of the
// kit's own tests, because it drives checkCloserContract directly. That is
// deliberate: the checker is a pure function precisely so its verdict can be
// observed here. The testing.T wrapper around it cannot be tested the same way,
// since a t.Fatal against a hand-built &testing.T{} calls runtime.Goexit rather
// than recording a failure anywhere this test could read it.
//
// Every branch of the checker has a test below that fails if that branch is
// deleted. That property is the point of the file and is worth re-verifying by
// mutation whenever a branch is added: a harness that gates every provider's
// Close is only worth what its own suite catches, and an assertion no test pins
// is indistinguishable from one that was never written.
package providertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// closeLeakProvider implements io.Closer but keeps resolving after Close, which
// the CloserContract case must reject.
type closeLeakProvider struct{ closed bool }

func (p *closeLeakProvider) Scheme() string { return "closeleak" }
func (p *closeLeakProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("still alive"), Version: "1"}, nil
}
func (p *closeLeakProvider) Close() error { p.closed = true; return nil }

// closeCleanProvider satisfies the contract.
type closeCleanProvider struct {
	mu     sync.Mutex
	closed bool
}

func (p *closeCleanProvider) Scheme() string { return "closeclean" }
func (p *closeCleanProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return mamori.Value{}, fmt.Errorf("%w: closeclean: provider is closed", mamori.ErrUnavailable)
	}
	return mamori.Value{Bytes: []byte("alive"), Version: "1"}, nil
}
func (p *closeCleanProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// brokenCloser is closeCleanProvider with exactly one defect switched on. One
// knobbed type rather than seven near-identical fakes, so each test below names
// the single branch of checkCloserContract it pins, and adding a branch means
// adding a knob and a test rather than another copy of this file.
type brokenCloser struct {
	mu       sync.Mutex
	closed   bool
	resolved bool
	closes   int

	// Exactly one of these is set per test.
	failCold    bool          // Close fails when nothing has resolved yet
	failWarm    bool          // the first Close after a Resolve fails
	failSecond  bool          // Close is not idempotent
	failResolve bool          // Resolve fails even before any Close
	afterErr    error         // post-close Resolve returns this instead of ErrUnavailable
	afterSleep  time.Duration // post-close Resolve dawdles this long first
	afterBlocks bool          // post-close Resolve blocks until the context expires
}

func (p *brokenCloser) Scheme() string { return "broken" }

func (p *brokenCloser) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	closed := p.closed
	p.resolved = true
	p.mu.Unlock()

	if !closed {
		if p.failResolve {
			return mamori.Value{}, fmt.Errorf("%w: broken: backend is down", mamori.ErrUnavailable)
		}
		return mamori.Value{Bytes: []byte("alive"), Version: "1"}, nil
	}
	if p.afterBlocks {
		// The gcp shape exactly: a dial that runs out the clock and reports the
		// timeout as unavailable. Judged on the sentinel alone, this passes.
		<-ctx.Done()
		return mamori.Value{}, fmt.Errorf("%w: broken: %w", mamori.ErrUnavailable, ctx.Err())
	}
	if p.afterSleep > 0 {
		time.Sleep(p.afterSleep)
	}
	if p.afterErr != nil {
		return mamori.Value{}, p.afterErr
	}
	return mamori.Value{}, fmt.Errorf("%w: broken: provider is closed", mamori.ErrUnavailable)
}

func (p *brokenCloser) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	switch {
	case p.failCold && !p.resolved:
		return errors.New("broken: close dialed with nothing to release")
	// resolved is load-bearing in this one: without it the cold instance's own
	// first Close trips this knob, the checker fails at the cold check instead,
	// and the warm-close branch this fake exists to reach goes unexecuted while
	// its test still passes.
	case p.failWarm && p.resolved && p.closes == 1:
		return errors.New("broken: close failed")
	case p.failSecond && p.closes > 1:
		return errors.New("broken: close is not idempotent")
	}
	p.closed = true
	return nil
}

// newBroken adapts a knobbed fake to the constructor checkCloserContract takes.
// Each call yields a fresh instance carrying the same knobs, which is what a
// real Config.New does.
func newBroken(set func(*brokenCloser)) func() mamori.Provider {
	return func() mamori.Provider {
		b := &brokenCloser{}
		set(b)
		return b
	}
}

func closerRef(t *testing.T, scheme string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(scheme + "://some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	return ref
}

func TestCheckCloserContractRejectsResolveAfterClose(t *testing.T) {
	ref := closerRef(t, "closeleak")
	err := checkCloserContract(func() mamori.Provider { return &closeLeakProvider{} }, ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider that resolves after Close")
	}
	// The message is asserted, not just the rejection, because the
	// sentinel-specificity check downstream rejects this provider too (a nil
	// error is not errors.Is anything). Rejection alone would therefore stay
	// green with this branch deleted, and the author of a leaking provider
	// would be told their error had the wrong sentinel rather than that they
	// returned a value.
	if !strings.Contains(err.Error(), "returned a value") {
		t.Fatalf("checkCloserContract rejected a resolve-after-Close provider with the wrong "+
			"diagnosis; want one naming the returned value, got: %v", err)
	}
}

func TestCheckCloserContractAcceptsACompliantProvider(t *testing.T) {
	ref := closerRef(t, "closeclean")
	if err := checkCloserContract(func() mamori.Provider { return &closeCleanProvider{} }, ref); err != nil {
		t.Fatalf("checkCloserContract rejected a compliant provider: %v", err)
	}
}

// The sentinel-specificity branch. Without it a provider may report anything at
// all after Close, including the not-found it could only have learned by
// consulting the backend it was told to release.
func TestCheckCloserContractRejectsAWrongSentinelAfterClose(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) {
		b.afterErr = fmt.Errorf("%w: gone", mamori.ErrNotFound)
	}), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider that reports ErrNotFound after Close")
	}
}

// The idempotency branch.
func TestCheckCloserContractRejectsANonIdempotentClose(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) { b.failSecond = true }), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider whose second Close fails")
	}
}

// The wall-clock branch, and the reason it exists: this provider reports
// exactly the sentinel the contract asks for and is still wrong, because it
// went to the backend to find out. Only its cost gives it away.
func TestCheckCloserContractRejectsASlowPostCloseResolve(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) {
		b.afterSleep = closedResolveBound + 150*time.Millisecond
	}), ref)
	if err == nil {
		t.Fatalf("checkCloserContract accepted a post-close Resolve slower than the %v bound; "+
			"a rebuilt client that dials and fails reports ErrUnavailable too", closedResolveBound)
	}
}

// The deadline branch. A post-close Resolve that runs out this case's own clock
// must fail the contract rather than be accepted on its sentinel: a timed-out
// dial classifies as unavailable in several providers, so folding the deadline
// into the accepted path would pass the defect this case exists to catch.
func TestCheckCloserContractRejectsAPostCloseResolveThatBlocks(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) { b.afterBlocks = true }), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a post-close Resolve that blocked until the deadline " +
			"and then reported the timeout as ErrUnavailable")
	}
	// As in the resolve-after-Close test, the message is asserted because the
	// wall-clock check downstream also rejects this provider: blocking to the
	// deadline necessarily overruns the bound. Rejection alone would stay green
	// with this branch deleted, and the deadline is the branch that has to keep
	// a timed-out dial out of the accepted ErrUnavailable path.
	if !strings.Contains(err.Error(), "deadline expired") {
		t.Fatalf("checkCloserContract rejected a blocking post-close Resolve with the wrong "+
			"diagnosis; want one naming the expired deadline, got: %v", err)
	}
}

// The cold-close branch: contract point two, that Close is safe on a provider
// that never resolved.
func TestCheckCloserContractRejectsAFailingColdClose(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) { b.failCold = true }), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider whose Close fails when nothing has resolved")
	}
}

// The warm-close branch, which the cold instance cannot stand in for.
func TestCheckCloserContractRejectsAFailingWarmClose(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) { b.failWarm = true }), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider whose Close fails after a successful Resolve")
	}
}

// notACloser is what most providers are: stateless, holding nothing to release.
type notACloser struct{}

func (notACloser) Scheme() string { return "notacloser" }
func (notACloser) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("alive"), Version: "1"}, nil
}

// The guard the testing.T wrapper relies on when it skips. The checker is
// documented as being called only for a provider already known to be a closer,
// so this branch is a backstop rather than a path the kit takes - which is
// exactly the kind of branch that rots into a nil dereference unpinned.
func TestCheckCloserContractRejectsANonCloser(t *testing.T) {
	ref := closerRef(t, "notacloser")
	if err := checkCloserContract(func() mamori.Provider { return notACloser{} }, ref); err == nil {
		t.Fatal("checkCloserContract accepted a provider that does not implement io.Closer")
	}
}

// The checker builds two instances, so a Config.New that is not consistent
// about what it returns has to be reported rather than panicked on.
func TestCheckCloserContractRejectsAnInconsistentConstructor(t *testing.T) {
	ref := closerRef(t, "closeclean")
	n := 0
	err := checkCloserContract(func() mamori.Provider {
		n++
		if n == 1 {
			return &closeCleanProvider{}
		}
		return notACloser{}
	}, ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a constructor that returned a closer and then a non-closer")
	}
}

// The pre-close resolve branch. Without it a provider whose Resolve never works
// at all reaches the post-close assertions and satisfies them for the wrong
// reason: a provider that always fails with ErrUnavailable fails with
// ErrUnavailable after Close too.
func TestCheckCloserContractRejectsAProviderThatCannotResolveAtAll(t *testing.T) {
	ref := closerRef(t, "broken")
	err := checkCloserContract(newBroken(func(b *brokenCloser) { b.failResolve = true }), ref)
	if err == nil {
		t.Fatal("checkCloserContract accepted a provider that could not resolve the seeded key " +
			"before Close, so the post-close assertions proved nothing")
	}
}
