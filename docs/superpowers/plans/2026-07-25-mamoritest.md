# Workstream C: the mamoritest consumer test kit

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let application authors test their `OnChange` handlers and error handling deterministically, without a real backend, via a scriptable in-memory provider and wait helpers.

**Architecture:** `providertest` serves provider authors; application authors have nothing. This adds `mamoritest`, a subpackage of core (like `net/http/httptest`), holding a scriptable `mamori.Provider` that implements `WatchableProvider` so changes are delivered natively, plus `WaitForSnapshot` (which polls the `Status().Snapshot` counter added in workstream B) and error-capture helpers. It takes `testing.TB`, adds no dependency, and tests itself against a real `mamori.Watch`.

**Tech Stack:** Go 1.26, stdlib (`testing`, `context`, `sync`, `time`). No new dependencies.

This implements spec section 8 (`docs/superpowers/specs/2026-07-24-operational-layer-design.md`). It depends on `Watcher.Status().Snapshot` from the observability-core plan (`2026-07-25-observability-core.md`, complete).

## Global Constraints

- **Core module, new subpackage `mamoritest/`.** It imports `testing` (like `httptest`) and the parent `mamori` package. No new external dependency.
- **Do not run `git commit`.** Stage with `git add`, report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command;** `make test` from the repo root.
- **The tree stays green after every task.**
- **No em-dash characters** anywhere.
- **Goroutine hygiene:** the scriptable provider must not leak goroutines. Its watch channels must close on context cancellation. The kit's own tests run under `goleak`.
- Doc comments on every exported symbol, explaining the why.

---

### Task 1: The scriptable provider

**Files:**
- Create: `mamoritest/mamoritest.go`
- Create: `mamoritest/mamoritest_test.go`

**Interfaces:**
- Consumes: `mamori.Provider`, `mamori.WatchableProvider`, `mamori.Ref`, `mamori.Value`, `mamori.Update`, `mamori.ErrNotFound`, `mamori.VersionHash`.
- Produces: `Provider`, `NewProvider(scheme string) *Provider`, and its methods `Set`, `SetBytes`, `Del`, `Fail`, `Clear`.

**Design.** The provider is a mutex-guarded map of key to value plus a map of key to injected error, and a set of per-key watch channels. `Resolve` returns the value, `ErrNotFound` for a deleted/missing key, or the injected error. `Watch` registers a channel for the ref's key and returns it; it emits a baseline immediately and closes on context cancellation (no leaked goroutine). `Set`/`SetBytes`/`Del`/`Fail`/`Clear` mutate the state and push an `Update` to every active watcher of that key. Model the watch mechanics on the existing in-repo `watchProvider` in `watch_test.go` (read it first, including its recently added `pushErr` helper).

- [ ] **Step 1: Write the failing tests**

Create `mamoritest/mamoritest_test.go`. These tests drive a REAL `mamori.Watch`, which is the point of the kit. Cover:
- `NewProvider` + `Set` a value, then `mamori.Load` over a config using that scheme resolves it.
- `Watch` over a config, then `Set` a new value, and assert (via the still-to-come `WaitForSnapshot`, or in this task via a direct `w.Status().Snapshot` poll loop with a deadline) that the watcher observes the change and `w.Get()` reflects it. Since `WaitForSnapshot` lands in Task 2, this task's watch test can poll `w.Status().Snapshot` inline; Task 2 replaces that with the helper.
- `Del` a key makes a subsequent `Load` of a required field fail with `ErrNotFound` (and a defaulted field fall back to its default).
- `Fail` a key makes `Resolve` return the injected error; `Clear` restores it.
- `goleak` on `w.Close()` after using the provider under `Watch`: no leaked goroutine.

Use `goleak.VerifyNone(t)` in a `TestMain` or per-test defer, matching how the parent package's tests use goleak (read `watch_test.go`).

- [ ] **Step 2: Run, confirm failure**

```bash
cd mamoritest && GOWORK=off go test ./... -v
```

Expected: `undefined: NewProvider`.

- [ ] **Step 3: Implement `mamoritest/mamoritest.go`**

```go
// Package mamoritest provides an in-memory, scriptable mamori.Provider for
// testing application code that consumes mamori. Where providertest serves
// provider authors, mamoritest serves application authors: it lets you drive a
// real mamori.Watch through value changes, deletions, and failures
// deterministically, without a real backend.
package mamoritest

import (
	"context"
	"sync"

	"github.com/xavidop/mamori"
)

// Provider is an in-memory, scriptable mamori.Provider. It implements
// mamori.WatchableProvider, so changes pushed with Set, Del, and Fail are
// delivered to a Watch natively rather than by polling. It is safe for
// concurrent use.
type Provider struct {
	scheme string
	mu     sync.Mutex
	values map[string][]byte
	fails  map[string]error
	seq    uint64
	subs   map[string][]chan mamori.Update
}

// NewProvider returns a scriptable provider registered for the given scheme.
// Pass it to Load or Watch with mamori.WithProvider.
func NewProvider(scheme string) *Provider {
	return &Provider{
		scheme: scheme,
		values: map[string][]byte{},
		fails:  map[string]error{},
		subs:   map[string][]chan mamori.Update{},
	}
}

// Scheme implements mamori.Provider.
func (p *Provider) Scheme() string { return p.scheme }
```

Implement:
- `Resolve(ctx, ref) (Value, error)`: honor `ctx.Err()` first (the `providertest` ContextCancel case requires it), then return the injected error for `ref.Path` if any, then `ErrNotFound` if the key is absent, else the value with a `Version` derived from `mamori.VersionHash` or an incrementing `seq` (use `VersionHash(bytes)` so equal values have equal versions, matching real providers).
- `Watch(ctx, ref) (<-chan mamori.Update, error)`: register a buffered channel under `ref.Path`, emit the current state as a baseline, and start a goroutine that closes/deregisters the channel on `ctx.Done()`. Match the leak-free pattern in `watch_test.go`'s `watchProvider`.
- `Set(key, val string)` and `SetBytes(key string, b []byte)`: store and push an `Update{Value: ...}` to all subscribers of `key`.
- `Del(key string)`: remove the value and push an `Update{Err: mamori.ErrNotFound}` (or a value-absent signal consistent with how the engine treats a deleted key; read how the engine's `handleErr` now tolerates `ErrNotFound` for default/optional fields, from the observability plan, and make `Del`'s pushed update consistent with that).
- `Fail(key string, err error)`: record the error and push an `Update{Err: err}` to subscribers.
- `Clear(key string)`: remove the injected error (subsequent resolves succeed or return `ErrNotFound` per the value's presence).

The push mechanism must not block on a slow or gone consumer: send with a `select` that also honors a per-subscription done signal, exactly as `watch_test.go` does.

- [ ] **Step 4: Run, confirm pass; race; goleak**

```bash
cd mamoritest && GOWORK=off go test ./... -v
cd mamoritest && GOWORK=off go test -race ./...
```

- [ ] **Step 5: Full suite; stage**

```bash
make test
git add mamoritest/mamoritest.go mamoritest/mamoritest_test.go
```

```
feat(mamoritest): add a scriptable in-memory provider for consumers

Application authors can now drive a real mamori.Watch through value changes,
deletions, and failures deterministically, without a real backend. The
provider implements WatchableProvider so changes are delivered natively, and
it leaks no goroutine on Close.
```

---

### Task 2: WaitForSnapshot, CaptureErrors, WaitForError

**Files:**
- Create: `mamoritest/wait.go`
- Modify: `mamoritest/mamoritest_test.go` (add wait-helper tests; replace Task 1's inline snapshot poll)

**Interfaces:**
- Consumes: `mamori.Watcher[T].Status()`, `mamori.OnError`, `mamori.Kind`, `mamori.ErrorKind`.
- Produces: `WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64)`, `CaptureErrors() (mamori.Option, *ErrorCapture)`, `ErrorCapture`, `WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error`.

**Design.** `WaitForSnapshot` polls `w.Status().Snapshot` until it reaches `v`, with a bounded deadline (e.g. 2s), calling `tb.Fatalf` with a clear message on timeout. This is deterministic without sleeps in the test body: the test sets a value, then waits for the snapshot version to advance. `CaptureErrors` returns a `mamori.OnError(...)` option whose callback appends to a thread-safe `ErrorCapture`, plus the capture. `WaitForError` blocks until the capture holds an error whose `mamori.ErrorKind` matches, or fails after the deadline; it returns the matched error.

- [ ] **Step 1: Write the failing tests**

Add to `mamoritest/mamoritest_test.go`:

```go
func TestWaitForSnapshotObservesChange(t *testing.T) {
	p := NewProvider("test")
	p.Set("db/password", "hunter2")
	type Config struct {
		Pw string `source:"test://db/password"`
	}
	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	p.Set("db/password", "rotated")
	WaitForSnapshot(t, w, 2)
	if got := w.Get().Pw; got != "rotated" {
		t.Fatalf("after WaitForSnapshot, Get().Pw = %q, want rotated", got)
	}
}

func TestWaitForErrorCapturesKind(t *testing.T) {
	p := NewProvider("test")
	p.Set("k", "v")
	type Config struct {
		K string `source:"test://k"`
	}
	onErr, errs := CaptureErrors()
	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p), onErr)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	p.Fail("k", mamori.ErrPermissionDenied)
	got := WaitForError(t, errs, mamori.KindPermissionDenied)
	if mamori.ErrorKind(got) != mamori.KindPermissionDenied {
		t.Fatalf("WaitForError returned kind %q, want permission_denied", mamori.ErrorKind(got))
	}
}
```

Add a timeout-cleanliness test: `WaitForSnapshot` for a version that never arrives must fail the test (verify with a `recordingTB`-style shim like `providertest` uses, so the expected failure does not fail the real test).

- [ ] **Step 2: Run, confirm failure**

```bash
cd mamoritest && GOWORK=off go test ./... -run 'TestWaitFor' -v
```

- [ ] **Step 3: Implement `mamoritest/wait.go`**

```go
package mamoritest

import (
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// defaultWait bounds how long the Wait helpers block before failing the test.
const defaultWait = 2 * time.Second

// WaitForSnapshot blocks until the watcher has applied snapshot version v, then
// returns. It fails the test if v is not reached within a bounded deadline. It
// is deterministic and does not sleep in the test body: set a value, then wait
// for the snapshot version to advance.
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64) {
	tb.Helper()
	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		if w.Status().Snapshot >= v {
			return
		}
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("mamoritest: snapshot version %d not reached within %s (current %d)",
		v, defaultWait, w.Status().Snapshot)
}

// ErrorCapture records errors delivered to OnError for later assertion.
type ErrorCapture struct {
	mu   sync.Mutex
	errs []error
}

// CaptureErrors returns an Option installing an OnError sink plus the capture it
// feeds. Pass the Option to Watch and assert against the capture with
// WaitForError.
func CaptureErrors() (mamori.Option, *ErrorCapture) {
	c := &ErrorCapture{}
	opt := mamori.OnError(func(err error) {
		c.mu.Lock()
		c.errs = append(c.errs, err)
		c.mu.Unlock()
	})
	return opt, c
}

// WaitForError blocks until the capture holds an error classified as kind, then
// returns it. It fails the test if no such error arrives within the deadline.
func WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error {
	tb.Helper()
	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, err := range c.errs {
			if mamori.ErrorKind(err) == kind {
				c.mu.Unlock()
				return err
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("mamoritest: no error of kind %q within %s", kind, defaultWait)
	return nil
}
```

Confirm `mamori.OnError` exists with signature `func(func(error)) Option` (read `reconciler.go`); it does. Confirm `WaitForSnapshot`'s polling of `Status().Snapshot` matches the version contract from workstream B (initial load is version 1, first applied change is 2).

- [ ] **Step 4: Run, confirm pass; race; full suite**

```bash
cd mamoritest && GOWORK=off go test ./... -v
cd mamoritest && GOWORK=off go test -race ./...
make test
```

- [ ] **Step 5: Stage**

```bash
git add mamoritest/wait.go mamoritest/mamoritest_test.go
```

```
feat(mamoritest): add WaitForSnapshot and error-capture helpers

WaitForSnapshot polls the snapshot version so a test can set a value and
deterministically wait for the watcher to apply it, no sleeps in the test
body. CaptureErrors and WaitForError let a test assert that a failing source
reaches OnError with the expected kind.
```

---

### Task 3: Documentation

**Files:**
- Create: `site/src/pages/docs/testing.md`
- Modify: `site/src/layouts/DocsLayout.astro` (nav entry)
- Modify: `README.md` (mention the test kit)

**Interfaces:** consumes Tasks 1-2.

- [ ] **Step 1: Write testing.md**

Create `site/src/pages/docs/testing.md` (read a sibling for frontmatter and voice). Cover:
- Why: testing an `OnChange` handler or error handling without a real backend.
- The scriptable `Provider`: `NewProvider`, `Set`/`SetBytes`/`Del`/`Fail`/`Clear`.
- `WaitForSnapshot` for deterministic change assertions.
- `CaptureErrors` + `WaitForError` for error-path assertions.
- A complete worked example: a test that sets a value, rotates it, `WaitForSnapshot`, and asserts the handler reacted; and one that `Fail`s a key and `WaitForError`s the kind.
- Mention `mamori.NewFakeClock` for tests that need to drive poll intervals, and cross-link `observability.md` (Doctor for CI preflight).

Verify every code example compiles against the real signatures.

- [ ] **Step 2: Nav, README, build**

Add `testing.md` to the nav in `DocsLayout.astro`. Add a test-kit mention to `README.md`.

```bash
make site-build   # Node 22; nvm use 22 if the engine check fails
```

- [ ] **Step 3: Stage**

```bash
git add site/src/pages/docs/testing.md site/src/layouts/DocsLayout.astro README.md
```

```
docs: document the mamoritest consumer test kit

Adds a testing page covering the scriptable provider and the WaitForSnapshot
and error-capture helpers, with worked examples for asserting an OnChange
handler reacts and that a failing source reaches OnError with the right kind.
```

---

## Self-Review

**Spec coverage.** Implements spec section 8 in full: the scriptable `Provider` with `Set`/`SetBytes`/`Del`/`Fail`/`Clear` (Task 1), and `WaitForSnapshot`/`CaptureErrors`/`WaitForError` (Task 2), documented (Task 3). `WaitForSnapshot` consumes the `Status().Snapshot` version counter from workstream B, which is why this plan follows it.

**Placeholders.** None. The provider and the wait helpers are given in full. Task 1's watch test uses an inline snapshot-poll that Task 2 replaces with `WaitForSnapshot`, which is called out explicitly rather than left implicit.

**Type consistency.** `Provider` implements both `mamori.Provider` and `mamori.WatchableProvider`. `WaitForSnapshot`, `CaptureErrors`, `WaitForError` use the exact `mamori` symbols they depend on (`Status().Snapshot`, `OnError`, `Kind`, `ErrorKind`), all of which exist. `Del`'s pushed update is made consistent with the engine's `ErrNotFound`-tolerance behavior added in the observability plan, so a deleted default/optional field does not spuriously fail a consumer's test.

**Risk noted.** The one risk is goroutine hygiene in the provider's `Watch`: a leaked watch goroutine would fail `goleak` and, worse, mislead consumers whose own tests then flake. The plan mandates modeling on the in-repo `watchProvider` (proven leak-free) and a `goleak`-on-Close test. The wait helpers poll rather than block on a channel, which is deliberate: it keeps them robust against missed signals and matches how the version counter is observed, at the cost of a 1ms poll granularity that is irrelevant to test correctness.
