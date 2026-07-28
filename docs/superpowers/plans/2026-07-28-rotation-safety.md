# Rotation Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an application prove a reconciled configuration actually works before it becomes current (`PreApply`), and let an operator force an immediate re-resolve (`Refresh`).

**Architecture:** `PreApply` is a gate inside the reconciler's existing `flush`, sitting between `buildCandidate` and the swap, reusing the engine's reject-and-keep-last-good path rather than adding state. `Refresh` is another command on the control channel `pin.go` already owns.

**Tech Stack:** Go 1.26, standard library only for new code. Tests use `testing`, the package's `FakeClock` with its `BlockUntil` handshake, and `go.uber.org/goleak`.

**Spec:** `docs/superpowers/specs/2026-07-28-rotation-safety-design.md`

## Global Constraints

- **No new dependencies in the core module.**
- **No change to `Provider`, `WatchableProvider`, or `BatchProvider`.**
- **No new route on the admin endpoint.** `Handler` serves exactly `GET /` and `GET /healthz`; every other path and method stays 404. This is decision D5.
- **No refresh verb in the config server's wire protocol**, and no upstream propagation from a client's `Refresh`. Decision D8. Do not touch `server/` or `providers/mamori/` in this plan.
- **A `PreApply` timeout is a rejection, never an acceptance.** Decision D4.
- **Error sentinels wrapped with `%w`, never `%v`**; two-verb form when wrapping both a sentinel and a cause.
- **Run `go test -race ./...` from the repo root before every commit.** `goleak` runs in this suite.
- **Conventional Commits.** `feat:` for the two features, `docs:` for documentation.
- **Branches and PRs go through the `gh stack` CLI only.** Never `git checkout -b` a stack layer, never `gh pr create`.

## The one thing that will bite you

`buildCandidate` (`reconciler.go:926`) is **not** pure. Its second loop advances `e.applied[spec.Path] = v.Version` for every changed field, as a side effect, before returning.

So if `PreApply` rejects a candidate *after* `buildCandidate` has run, every rejected field now looks already-applied. The next `flush` computes an empty diff, returns `ok == false`, and **the rejected value is never retried, ever**, even when the field changes again to that same version. `Get()` silently serves the old config forever with no further errors.

Every rejection path in Task 3 must therefore roll `e.applied` back. `fields` carries exactly what is needed: each `FieldChange` has both `Path` and `OldVersion`. Task 3 Step 1 tests this specifically, because it is invisible in a single-update test.

## Delivery: two stacked PRs

The stack currently ends at `xavier/ref-grammar-interpolation` (PR #60). You are on `xavier/rotation-preapply`, created above it.

| PR | Branch | Tasks |
|---|---|---|
| 5 | `xavier/rotation-preapply` | 1, 2, 3, 4, 5 |
| 6 | `xavier/rotation-refresh` | 6, 7 |

`PreApply` touches `reconciler.go`'s apply path and is the riskier half, so it ships alone.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `preapply.go` | **Create** | The `PreApply` and `WithPreApplyTimeout` options, the `PreApplyError` type, and the `runPreApply` helper. |
| `preapply_test.go` | **Create** | Gate semantics: accept, reject, rollback, timeout, pinned, reentrancy. |
| `reconcile.go` | Modify | `options` gains `preApply any` and `preApplyTimeout time.Duration`; `defaultOptions` sets the timeout; `loadValue` runs the gate on the initial load. |
| `reconciler.go` | Modify | `engine` gains a typed `preApply`; `Watch` asserts it; `flush` calls it and rolls back on rejection. |
| `refresh.go` | **Create** | `Refresh`, its control-channel command, and the engine handler. |
| `refresh_test.go` | **Create** | Forced re-resolve, rejection propagation, closed watcher, concurrency. |
| `pin.go` | Modify | `pinKind` gains a `refresh` case so one control channel carries both. |
| `site/src/pages/docs/usage/rotation.md` | **Create** | The rotation-safety page. |

---

## Task 1: Commit the spec

**Files:**
- Create: `docs/superpowers/specs/2026-07-28-rotation-safety-design.md` (already written, currently untracked)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. This exists so the design is in the PR that implements it.

- [ ] **Step 1: Confirm the spec is present and untracked**

Run: `git status --porcelain docs/superpowers/specs/2026-07-28-rotation-safety-design.md`
Expected: `?? docs/superpowers/specs/2026-07-28-rotation-safety-design.md`

- [ ] **Step 2: Commit it**

```bash
git add docs/superpowers/specs/2026-07-28-rotation-safety-design.md
git commit -m "docs: spec for rotation safety (PreApply gate and Refresh)

PreApply gates a reconciled snapshot before it becomes current, so an
application can prove a rotated credential actually works rather than
discovering it in the request path. Refresh forces an immediate re-resolve.

Scope was cut on review: no dual-credential machinery, since WithHistory(1)
plus History() already retains the previous validated snapshot.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: The `PreApply` option, timeout, and error type

**Files:**
- Create: `preapply.go`
- Create: `preapply_test.go`
- Modify: `reconcile.go` (the `options` struct and `defaultOptions`)

**Interfaces:**
- Consumes: `Change[T]` (`reconciler.go`), `options` (`reconcile.go`).
- Produces, used by Tasks 3 and 4:
  - `func PreApply[T any](fn func(ctx context.Context, ev Change[T]) error) Option`
  - `func WithPreApplyTimeout(d time.Duration) Option`
  - `type PreApplyError struct{ Err error }` with `Error() string` and `Unwrap() error`
  - `options.preApply any` and `options.preApplyTimeout time.Duration`
  - `const defaultPreApplyTimeout = 10 * time.Second`

**This task wires nothing.** The gate is not called from anywhere until Task 3. Do not call it from `flush` here.

- [ ] **Step 1: Write the failing tests**

Create `preapply_test.go`:

```go
package mamori

import (
	"context"
	"errors"
	"testing"
	"time"
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestPreApply' . -v`
Expected: FAIL, compilation errors: `undefined: PreApply`, `undefined: WithPreApplyTimeout`, `undefined: PreApplyError`, `o.preApply undefined`, `o.preApplyTimeout undefined`, `undefined: defaultPreApplyTimeout`.

- [ ] **Step 3: Write `preapply.go`**

```go
package mamori

import (
	"context"
	"fmt"
	"time"
)

// defaultPreApplyTimeout bounds a PreApply hook when the caller sets none.
// Ten seconds is generous for the checks this hook exists for - opening a
// connection, exchanging a token - while still being short enough that a hook
// which hangs on an unresponsive backend does not stall reconciliation for
// long. See WithPreApplyTimeout for why the bound is mandatory.
const defaultPreApplyTimeout = 10 * time.Second

// PreApply installs a gate that runs before a reconciled snapshot becomes
// current. Returning a non-nil error rejects the candidate: Get keeps returning
// the last valid configuration, OnChange does not fire, and OnError receives a
// *PreApplyError describing the rejection.
//
// It exists for the checks struct validation cannot express, because they need
// I/O: that a rotated database password actually opens a connection, that a new
// API token is accepted by its issuer, that a reissued certificate chains to a
// trusted root. Validation answers "is this well-formed". PreApply answers
// "does this actually work", which is the question a rotation actually turns on.
//
//	w, err := mamori.Watch[Config](ctx,
//	    mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
//	        if !ev.Changed("DBPassword") {
//	            return nil
//	        }
//	        return pool.Ping(ctx, ev.New.DBPassword.Reveal())
//	    }),
//	)
//
// The hook runs on the reconciler goroutine, because it has to complete before
// the swap and the OnChange dispatch queue is asynchronous and lossy by design
// (WithQueueDepth drops the oldest event when full); a gate cannot be delivered
// on a channel that is allowed to drop. Two consequences follow, and both
// matter:
//
// It is bounded by WithPreApplyTimeout, and the bound cannot be removed.
//
// It must not call back into the same Watcher. Get is lock-free and safe, but
// Refresh, Pin, PinCurrent, and Unpin are serviced by the very goroutine the
// hook is occupying, so calling one deadlocks until the timeout fires.
//
// It is typed to the same T passed to Watch, and runs on the initial load as
// well as on every subsequent update, so a credential that does not work is
// caught at startup rather than at the first rotation.
func PreApply[T any](fn func(ctx context.Context, ev Change[T]) error) Option {
	return func(o *options) { o.preApply = fn }
}

// WithPreApplyTimeout bounds how long a PreApply hook may run, defaulting to
// defaultPreApplyTimeout.
//
// The bound is mandatory rather than optional because the hook runs on the
// reconciler goroutine, which also services every other field's updates, the
// published Status report, and pin/unpin commands. An unbounded hook would
// wedge all of that.
//
// Exceeding the budget is a REJECTION, not an acceptance: on timeout mamori
// does not know whether the new configuration works, and applying it anyway
// would defeat the point of having a gate. A hook that always times out
// therefore stalls updates - loudly, emitting a *PreApplyError once per
// attempt - rather than quietly serving unverified configuration. That is the
// intended trade.
func WithPreApplyTimeout(d time.Duration) Option {
	return func(o *options) { o.preApplyTimeout = d }
}

// PreApplyError is delivered to OnError when a PreApply hook rejects a
// candidate snapshot, and returned by Watch and Load when the hook rejects the
// initial configuration. Err is the hook's own error, or
// context.DeadlineExceeded when it exceeded WithPreApplyTimeout.
type PreApplyError struct {
	Err error
}

func (e *PreApplyError) Error() string {
	return fmt.Sprintf("mamori: pre-apply check rejected the configuration: %v", e.Err)
}

// Unwrap lets errors.Is and errors.As reach the hook's own error, including
// context.DeadlineExceeded for a hook that exceeded its budget.
func (e *PreApplyError) Unwrap() error { return e.Err }

// runPreApply invokes a typed hook under its timeout and wraps any refusal in
// a *PreApplyError. It returns nil when there is no hook, which is the common
// case, so callers need no nil check of their own.
//
// parent is the watcher's context, so cancelling the watcher also releases a
// hook blocked on a hanging backend rather than waiting out the full budget.
func runPreApply[T any](parent context.Context, hook func(context.Context, Change[T]) error, timeout time.Duration, ev Change[T]) error {
	if hook == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := hook(ctx, ev); err != nil {
		return &PreApplyError{Err: err}
	}
	// A hook that returned nil after its deadline passed did not actually
	// verify anything against a live deadline, so treat the expiry itself as
	// the refusal rather than trusting a result produced past the budget.
	if err := ctx.Err(); err != nil {
		return &PreApplyError{Err: err}
	}
	return nil
}
```

- [ ] **Step 4: Add the two fields to `options`**

In `reconcile.go`, add to the `options` struct immediately after `historyN`:

```go
	// preApply is the gate run before a candidate snapshot becomes current,
	// typed per T and stored as any exactly like onChange above; Watch[T]
	// asserts it. preApplyTimeout bounds it (see WithPreApplyTimeout).
	preApply        any
	preApplyTimeout time.Duration
```

And in `defaultOptions`, add:

```go
		preApplyTimeout: defaultPreApplyTimeout,
```

- [ ] **Step 5: Run to verify pass**

Run: `go test -race -run 'TestPreApply' . -v`
Expected: PASS, all three tests.

- [ ] **Step 6: Run the full suite and commit**

```bash
go test -race ./...
git add preapply.go preapply_test.go reconcile.go
git commit -m "feat: PreApply option, timeout, and error type

Adds the gate's public surface without wiring it to anything: PreApply stores a
hook typed per T the same way OnChange does, WithPreApplyTimeout bounds it at a
10s default, and PreApplyError carries a rejection to OnError while unwrapping
to the hook's own error.

The timeout is mandatory because the hook will run on the reconciler goroutine,
and exceeding it is a rejection rather than an acceptance: on timeout mamori
does not know whether the configuration works, and applying it anyway would
defeat the gate.

Wiring into the reconciler is the next commit.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Wire the gate into `flush`, with the rollback

**Files:**
- Modify: `reconciler.go` (`Watch`'s assertion around line 173, the `engine` struct around line 258, and `flush` around line 976)
- Modify: `preapply_test.go`

**Interfaces:**
- Consumes: `PreApply`, `WithPreApplyTimeout`, `PreApplyError`, `runPreApply` from Task 2.
- Produces: `engine[T].preApply func(context.Context, Change[T]) error`.

**This is the correctness-critical task.** Read "The one thing that will bite you" above before starting. `buildCandidate` mutates `e.applied` before returning, so every rejection path here must roll it back or the rejected value is never retried.

- [ ] **Step 1: Write the failing tests**

Append to `preapply_test.go`:

```go
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
// Here the hook rejects "bad" and accepts anything else. After the rejection,
// a THIRD value arrives. Without the rollback the engine has already recorded
// v2 as applied, so v3's diff still computes, and this test would pass anyway -
// which is why the assertion that matters is the recovery to "good" AND the
// absence of a second error.
func TestPreApplyRejectionIsRetriedOnNextChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"pr://a"`
	}
	clk := NewFakeClock(time.Time{})
	p := newWatchProvider("pr")
	p.set("a", "first", "v1")

	changed := make(chan cfg, 4)
	errs := make(chan error, 4)

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "bad" {
				return errors.New("rejected")
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

	p.push("a", "bad", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection for the bad value")
	}

	// The rejected version must not be recorded as applied. Re-push the SAME
	// version that was rejected, then a good one, and require recovery.
	p.push("a", "good", "v3")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case got := <-changed:
		if got.A != "good" {
			t.Errorf("after recovery A = %q, want good", got.A)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not recover after a rejection")
	}
	if got := w.Get().A; got != "good" {
		t.Errorf("Get().A = %q, want good", got)
	}
}

// TestPreApplyRollsBackAppliedVersions asserts the rollback directly against
// engine state, since the behavioral test above cannot distinguish "rolled
// back" from "the next diff happened to compute anyway".
func TestPreApplyRollsBackAppliedVersions(t *testing.T) {
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
```

Add `"sync/atomic"` and `"go.uber.org/goleak"` to `preapply_test.go`'s imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test -race -run 'TestPreApplyRejection|TestPreApplyRollsBack|TestPreApplyTimeout|TestPreApplyNotCalled' . -v`
Expected: FAIL. Nothing calls the hook yet, so `TestPreApplyRejectionKeepsLastGood` fails on `Get().A = "second"` and no error is delivered.

- [ ] **Step 3: Assert the typed hook in `Watch`**

In `reconciler.go`, beside the existing `onChange` assertion (around line 173):

```go
	var preApply func(context.Context, Change[T]) error
	if o.preApply != nil {
		preApply, _ = o.preApply.(func(context.Context, Change[T]) error)
	}
```

Add it to the `engine` literal alongside `onChange: onChange,`:

```go
		preApply: preApply,
```

And to the `engine` struct beside `onChange func(Change[T])`:

```go
	// preApply gates a candidate before it becomes current. Typed here the
	// same way onChange is; nil when the caller installed no hook.
	preApply func(context.Context, Change[T]) error
```

- [ ] **Step 4: Call the gate in `flush`, and roll back on rejection**

In `reconciler.go`'s `flush`, replace:

```go
	cand, fields, ok := e.buildCandidate()
	if !ok {
		return
	}

	old := e.lastGood
```

with:

```go
	cand, fields, ok := e.buildCandidate()
	if !ok {
		return
	}

	// Gate the candidate before anything observable changes. This runs after
	// validation (cheap and pure) and before the swap, so a network round trip
	// is only ever spent on a candidate whose shape is already known good.
	//
	// The rollback is not optional. buildCandidate advanced e.applied for every
	// changed field as a side effect before returning, so a rejection that left
	// it advanced would make the next flush compute an empty diff: the rejected
	// value would never be retried and Get would serve the old config forever,
	// silently. fields carries each field's OldVersion precisely so this can be
	// undone.
	if err := runPreApply(e.ctx, e.preApply, e.o.preApplyTimeout, Change[T]{
		Old:    e.lastGood,
		New:    cand,
		Fields: fields,
	}); err != nil {
		for _, f := range fields {
			e.applied[f.Path] = f.OldVersion
		}
		e.emitErr(err)
		return
	}

	old := e.lastGood
```

If the engine's watcher context is not already reachable as `e.ctx`, use whatever field the engine holds it in; check the `engine` struct and the `start(ctx)` signature, and thread it if it is only a parameter today. Say which you did in your report.

- [ ] **Step 5: Run to verify pass**

Run: `go test -race -run 'TestPreApply' . -v`
Expected: PASS, all tests.

- [ ] **Step 6: Run the full suite and commit**

```bash
go test -race ./...
git add reconciler.go preapply_test.go
git commit -m "feat: gate reconciled snapshots on PreApply before the swap

flush now runs the PreApply hook between buildCandidate and the swap. A
rejection emits a *PreApplyError to OnError and leaves Get, OnChange, and the
served snapshot exactly as they were, reusing the same reject-and-keep-last-good
outcome a validation failure already produces.

The rollback of e.applied is the load-bearing part. buildCandidate advances it
for every changed field as a side effect before returning, so a rejection that
left it advanced would make the next flush compute an empty diff: the rejected
value would never be retried and Get would serve stale config forever, with no
further errors. fields carries each field's OldVersion exactly so this can be
undone, and TestPreApplyRejectionIsRetriedOnNextChange pins it.

A timeout is a rejection, not an acceptance, and a hook that returns nil after
its deadline passed is treated as a refusal too: it did not verify anything
against a live deadline.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Run the gate on the initial load

**Files:**
- Modify: `reconcile.go` (`loadValue`, around line 168)
- Modify: `preapply_test.go`

**Interfaces:**
- Consumes: `runPreApply`, `PreApplyError` from Task 2.
- Produces: nothing new.

Decision D7: a hook that verifies a credential should verify the first one too. Discovering at startup that the configured credential does not work beats discovering it at the first rotation.

- [ ] **Step 1: Write the failing tests**

Append to `preapply_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -race -run 'TestPreApplyRejectsInitial|TestPreApplyRejectsLoad|TestPreApplyInitialLoad' . -v`
Expected: FAIL. `Watch` and `Load` currently succeed because nothing runs the hook on that path.

- [ ] **Step 3: Run the gate in `loadValue`**

In `reconcile.go`, in `loadValue`, after validation succeeds and before the successful return:

```go
	if err := o.validator.Validate(cfg); err != nil {
		return cfg, nil, &ValidationError{Err: err}
	}
	// Gate the initial configuration too (decision D7): a hook that verifies a
	// credential should verify the first one, so a credential that does not
	// work fails at startup rather than at the first rotation. Old is the zero
	// value of T, since nothing was serving yet, and Fields is nil for the
	// same reason: every field is new, so there is no prior version to diff
	// against and ev.Changed reports false for all of them.
	if hook, ok := o.preApply.(func(context.Context, Change[T]) error); ok {
		if err := runPreApply(ctx, hook, o.preApplyTimeout, Change[T]{New: cfg}); err != nil {
			return cfg, nil, err
		}
	}
	return cfg, res, nil
```

Note the type assertion is the comma-ok form and silently skips a hook typed for a different `T`, matching how `Watch` treats `onChange`.

- [ ] **Step 4: Run to verify pass**

Run: `go test -race -run 'TestPreApply' . -v`
Expected: PASS, all tests.

- [ ] **Step 5: Run the full suite and commit**

```bash
go test -race ./...
git add reconcile.go preapply_test.go
git commit -m "feat: run the PreApply gate on the initial load

Watch is already fail-fast on its initial Load, and a hook that verifies a
credential should verify the first one: discovering at startup that the
configured credential does not work beats discovering it at the first rotation.
Load runs it too, since it accepts the same Option type and the check is just
as useful for a one-shot resolve.

On this path ev.Old is the zero value of T and ev.Fields is nil, because
nothing was serving yet and every field is new, so ev.Changed reports false for
all of them. Pinned by TestPreApplyInitialLoadReceivesZeroOld.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Documentation for `PreApply`, and publish PR 5

**Files:**
- Create: `site/src/pages/docs/usage/rotation.md`
- Modify: `site/src/pages/docs/usage/index.md`, `site/src/pages/docs/usage/watching.md`, `site/src/pages/docs/usage/snapshots.md`, `README.md`, `doc.go`, `skills/mamori/SKILL.md`

- [ ] **Step 1: Read a neighbouring page first**

Run: `head -40 site/src/pages/docs/usage/watching.md`
Copy its frontmatter shape, heading levels, and code-fence style exactly.

- [ ] **Step 2: Create `site/src/pages/docs/usage/rotation.md`**

Cover, with runnable Go:
- The problem: `OnChange` is a notification, not a gate, and by the time it fires `Get()` already returns the new value.
- `PreApply` with the `ev.Changed` guard, since verifying on every unrelated field change is the mistake people will make.
- What a rejection does: `Get()` unchanged, `OnChange` silent, `*PreApplyError` to `OnError`, and the next change producing a fresh candidate.
- The timeout, that it defaults to 10s, and **that exceeding it is a rejection**.
- The reentrancy hazard: `Get` is safe inside a hook, `Refresh`/`Pin`/`Unpin` deadlock.
- That it runs on the initial load and a rejection fails `Watch`.
- **The credential-overlap pattern** from spec section 7, with the memory caveat stated as plainly as the benefit: retained snapshots hold rotated secrets for as long as they are retained, which is why `WithHistory` defaults to 0.
- A placeholder section for `Refresh`, one honest sentence saying it is not yet released. No mechanics, no `TODO` marker.

- [ ] **Step 3: Update the rest**

- `usage/index.md`: `PreApply` in the option walkthrough.
- `usage/watching.md`: where the gate sits in the reconcile cycle.
- `usage/snapshots.md`: cross-link the overlap pattern from `WithHistory`.
- `README.md`: a rotation-safety bullet, and `PreApply` in the `Watch` example.
- `doc.go`: the reconcile cycle now has a gate.
- `skills/mamori/SKILL.md`: `PreApply`, the timeout, and the reentrancy hazard.

- [ ] **Step 4: Verify the site builds**

```bash
export NVM_DIR="$HOME/.nvm"; . "$NVM_DIR/nvm.sh"; nvm use 22
cd site && npm run build && npm run linkcheck && cd ..
```
Expected: build succeeds, no broken links.

- [ ] **Step 5: Commit and publish**

```bash
git add -A
git commit -m "docs: rotation safety and the PreApply gate

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
gh stack submit --auto --open
gh stack view
```

Confirm the new PR's base is `xavier/ref-grammar-interpolation`, not `main`.

---

## Task 6: `Refresh`

**Files:**
- Create: `refresh.go`
- Create: `refresh_test.go`
- Modify: `pin.go` (the `pinKind` constants)
- Modify: `reconciler.go` (`handlePin`)

**Interfaces:**
- Consumes: `pinCmd`, `pinReply`, `sendPin`, `errWatcherClosed` (`pin.go:61`, `errors.go:27`); `engine.resolveAll` behavior via the existing seed path.
- Produces: `func (w *Watcher[T]) Refresh(ctx context.Context) error`.

**Reuse the existing control channel.** `pin.go` already carries commands to the reconciler goroutine and answers on a reply channel, falling back to `errWatcherClosed` when the watcher is gone. `Refresh` is another command on that same channel. Do not build a second control plane, and do not add a new sentinel.

- [ ] **Step 1: Add the branch to `gh stack`**

```bash
gh stack add xavier/rotation-refresh
```

- [ ] **Step 2: Write the failing tests**

Create `refresh_test.go`:

```go
package mamori

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestRefreshAppliesChangeBetweenPolls sets the poll interval to an hour, so a
// pass cannot come from a tick: only Refresh can produce the new value.
func TestRefreshAppliesChangeBetweenPolls(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rf://a"`
	}
	p := newWatchProvider("rf")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("a", "second", "v2") // silent: no push, so nothing is watching it

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second (Refresh must force a re-resolve)", got)
	}
}

func TestRefreshReturnsRejectionReason(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rr://a"`
	}
	p := newWatchProvider("rr")
	p.set("a", "first", "v1")
	boom := errors.New("rejected by the gate")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithPollInterval(time.Hour),
		PreApply(func(_ context.Context, ev Change[cfg]) error {
			if ev.New.A == "second" {
				return boom
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("a", "second", "v2")

	err = w.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh must return the rejection reason")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want one wrapping the hook's error", err)
	}
	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first", got)
	}
}

func TestRefreshAfterCloseReturnsClosed(t *testing.T) {
	type cfg struct {
		A string `source:"rc://a"`
	}
	p := newWatchProvider("rc")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_ = w.Close()

	if err := w.Refresh(context.Background()); !errors.Is(err, errWatcherClosed) {
		t.Errorf("err = %v, want errWatcherClosed", err)
	}
}

func TestRefreshHonorsCallerContext(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rx://a"`
	}
	p := newWatchProvider("rx")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRefreshIsConcurrencySafe(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"rk://a"`
	}
	p := newWatchProvider("rk")
	p.set("a", "only", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() { defer close(done); _ = w.Refresh(context.Background()) }()
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Refresh calls did not complete")
	}
}
```

Note `TestRefreshIsConcurrencySafe` closes `done` from eight goroutines, which panics. Fix it while writing: use a `sync.WaitGroup` and close `done` once after `wg.Wait()` in a separate goroutine. This is deliberately left as a real bug for you to catch and correct rather than copy blindly; say in your report that you did.

- [ ] **Step 3: Run to verify failure**

Run: `go test -race -run 'TestRefresh' . -v`
Expected: FAIL, compilation error: `w.Refresh undefined`.

- [ ] **Step 4: Add the `refresh` command kind**

In `pin.go`, extend `pinKind`:

```go
const (
	pinAt      pinKind = iota // Pin(version): freeze at a specific retained snapshot
	pinCurrent                // PinCurrent(): freeze at whatever Get returns right now
	unpin                     // Unpin(): resume, applying the newest snapshot
	refresh                   // Refresh(): re-resolve every field now and apply the result
)
```

- [ ] **Step 5: Write `refresh.go`**

```go
package mamori

import "context"

// Refresh forces an immediate re-resolve of every field, bypassing poll
// intervals and per-ref backoff, and blocks until the resulting snapshot has
// been applied or rejected.
//
// It returns nil when a snapshot was applied and when nothing changed, and the
// rejection reason when the candidate failed validation or a PreApply gate. A
// SIGHUP handler therefore learns whether the reload actually worked, which is
// the whole reason this blocks rather than queueing.
//
// It does NOT bypass PreApply. A forced refresh is still gated; that is the
// point of having a gate.
//
// Refresh is delivered to the reconciler goroutine over the same control
// channel Pin, PinCurrent, and Unpin use, so it serializes with normal
// reconciliation rather than racing it, and it answers errWatcherClosed after
// Close for the same reason they do (see sendPin in pin.go). Do not call it
// from inside a PreApply hook: that hook runs on the goroutine which would
// have to service this command, so it deadlocks until the hook's timeout fires.
//
// For a field resolved through a mamori:// ref, Refresh re-reads the config
// server's current value. It does not make the server re-resolve its own
// upstream: the server exists so that N consumers cost one upstream watch
// rather than N, and letting any client force an upstream fetch would invert
// exactly that property.
func (w *Watcher[T]) Refresh(ctx context.Context) error {
	cmd := pinCmd{kind: refresh, reply: make(chan pinReply, 1)}
	select {
	case w.control <- cmd:
	case <-w.ctx.Done():
		return errWatcherClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case rep := <-cmd.reply:
		return rep.err
	case <-w.ctx.Done():
		return errWatcherClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

- [ ] **Step 6: Handle it in the reconciler**

In `reconciler.go`'s `handlePin`, add a `refresh` case that re-resolves every spec and flushes, replying with the flush's outcome.

Read `handlePin`'s existing cases and `engine.start`'s seeding code first, and reuse whichever resolve helper they already use rather than writing a third path. The command must reply exactly once on every branch, including the error branches, or `Refresh` blocks until the watcher closes.

- [ ] **Step 7: Run to verify pass**

Run: `go test -race -run 'TestRefresh' . -v`
Expected: PASS, all tests.

- [ ] **Step 8: Run the full suite and commit**

```bash
go test -race ./...
git add refresh.go refresh_test.go pin.go reconciler.go
git commit -m "feat: Refresh forces an immediate re-resolve

Refresh re-resolves every field now, bypassing poll intervals and per-ref
backoff, and blocks until the result is applied or rejected so a SIGHUP handler
learns whether the reload worked.

It is another command on the control channel pin.go already owns, so it
serializes with normal reconciliation rather than racing it and answers
errWatcherClosed after Close exactly as Pin does. No second control plane and
no new sentinel.

It does not bypass PreApply, and for a mamori:// field it re-reads the config
server rather than making the server re-resolve upstream (decision D8).

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Documentation for `Refresh`, and publish PR 6

**Files:**
- Modify: `site/src/pages/docs/usage/rotation.md`, `site/src/pages/docs/observability/admin.md`, `site/src/pages/docs/server/index.md`, `README.md`, `doc.go`, `skills/mamori/SKILL.md`

- [ ] **Step 1: Replace the `Refresh` placeholder in `rotation.md`**

Real content: what it does, that it blocks and why, that it returns the rejection reason, the SIGHUP example, that it does not bypass `PreApply`, and the reentrancy hazard.

- [ ] **Step 2: Record the two boundaries**

- `observability/admin.md`: why there is no `POST /refresh` (D5) and how to mount your own handler calling `w.Refresh` with your own authorization.
- `server/index.md`: what `Refresh` means for a `mamori://` consumer (D8) — it re-reads the server, it does not force the server upstream, and why that boundary exists. State the amplification reasoning: N clients across M bindings would become N×M on-demand calls against rate-limited, per-call-billed backends, triggerable by anyone merely authorized to read.

- [ ] **Step 3: Update the rest**

`README.md`, `doc.go`, and `skills/mamori/SKILL.md` gain `Refresh`.

- [ ] **Step 4: Verify the site builds**

```bash
export NVM_DIR="$HOME/.nvm"; . "$NVM_DIR/nvm.sh"; nvm use 22
cd site && npm run build && npm run linkcheck && cd ..
```

- [ ] **Step 5: Commit and publish**

```bash
git add -A
git commit -m "docs: Refresh, and the two boundaries it does not cross

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
gh stack submit --auto --open
gh stack view
```

Confirm the new PR's base is `xavier/rotation-preapply`.

---

## Self-Review

**Spec coverage:**

| Spec section | Task |
|---|---|
| 5.1 API | 2 |
| 5.2 semantics (order, `ev.Changed`, ctx, rejection effects) | 3 |
| 5.2 initial load (D7) | 4 |
| 5.2 runs while pinned | 3 (`flush` gates before the pinned branch, so it holds by construction; the test is in Task 3's list) |
| 5.3 reentrancy hazard | 2 (doc comment), 6 (`Refresh` doc comment) |
| 6 `Refresh` | 6 |
| 7 overlap pattern | 5 (documentation only, no code, as specified) |
| 8 testing | 3, 4, 6 |
| 9 documentation | 5, 7 |
| 10 delivery | branch and submit steps in 5, 6, 7 |
| D1-D4 | 2, 3 |
| D5 | 7 (documented; no code by definition) |
| D6 | 6 |
| D7 | 4 |
| D8 | 6 (doc comment), 7 (docs site) |

Every spec section maps to a task. D5 and section 7 are documentation-only by design, which is stated rather than left as a gap.

**Placeholder scan:** No TBD or TODO. Two deliberate exceptions, both explicit: the `Refresh` placeholder section created in Task 5 Step 2 (required to be a real heading plus one sentence, filled in by Task 7) and the seeded bug in Task 6 Step 2's concurrency test, which is called out in the step itself and required to be reported.

**Type consistency:** `runPreApply(context.Context, func(context.Context, Change[T]) error, time.Duration, Change[T]) error` is defined in Task 2 and called with that signature in Tasks 3 and 4. `PreApplyError` is defined in Task 2 and asserted in Tasks 3, 4, and 6. `options.preApply`/`preApplyTimeout` are added in Task 2 and read in Tasks 3 and 4. `engine[T].preApply` is added in Task 3 and used only there. `pinCmd`/`pinReply`/`errWatcherClosed` are pre-existing and used unchanged in Task 6; `pinKind` gains exactly one constant.

**One deliberate under-specification:** Task 6 Step 6 does not give the `handlePin` refresh case verbatim, because the right implementation depends on which resolve helper `engine.start` and `handlePin` already share, and inventing a third path would be worse than reading the two that exist. The step says so and states the invariant that matters: reply exactly once on every branch.
