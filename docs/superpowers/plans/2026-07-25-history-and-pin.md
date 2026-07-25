# Workstream D: snapshot history and Pin/Unpin

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator freeze a running `Watcher` at a known-good snapshot ("freeze config, I am debugging prod") and inspect recent snapshots, without losing the atomic-reconciliation guarantees.

**Architecture:** The engine already assigns a monotonic snapshot version (workstream B). This adds a bounded snapshot history the engine publishes through an `atomic.Pointer` (like the report), and Pin/Unpin/PinCurrent, which route through a control channel into the reconciler goroutine so that pinning, unpinning, and the single coalesced `Change` that Unpin emits all stay on the one goroutine that owns config application and `OnChange` delivery. Reads (`History`, `Pinned`, `Status`) stay lock-free.

**Tech Stack:** Go 1.26, stdlib only. No new dependencies.

This implements spec section 9 (`docs/superpowers/specs/2026-07-24-operational-layer-design.md`). It builds on the snapshot version counter from the observability-core plan (complete): version starts at 1 and increments once per applied non-empty diff.

## Global Constraints

- **Core dependencies frozen.** stdlib plus the four allowed deps. This plan adds nothing.
- **Do not run `git commit`.** Stage with `git add`, report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command;** `make test` from the repo root.
- **The tree stays green after every task.**
- **No em-dash characters** anywhere.
- **The invariant that must survive this work:** `OnChange` is delivered serially on the single reconciler goroutine, and `Get()` never returns a partially-applied or validation-failing snapshot. Pin/Unpin must not violate either. All new tests run under `-race`; `goleak` on `Close` must still pass.
- **Secret retention is a documented tradeoff.** Retained snapshots hold full `T` copies, including rotated secret material. History defaults to off (0) for exactly this reason. The docs must say so.
- Doc comments on every exported symbol, explaining the why.

---

### Task 1: Snapshot history

**Files:**
- Create: `history.go`
- Create: `history_test.go`
- Modify: `reconcile.go` (add `historyN` to `options`, `WithHistory`)
- Modify: `reconciler.go` (engine holds and publishes history)
- Modify: `errors.go` (`ErrNoSuchSnapshot`)

**Interfaces:**
- Consumes: the version counter and `FieldChange` (workstream B / existing).
- Produces: `Snapshot[T]`, `WithHistory(n int) Option`, `Watcher.History() []Snapshot[T]`, `ErrNoSuchSnapshot`. Task 2 (Pin) consumes `History` and `ErrNoSuchSnapshot`.

**Design.** The engine keeps a bounded slice of `Snapshot[T]` (newest last internally, returned newest-first) and republishes it through `w.snapshots atomic.Pointer[[]Snapshot[T]]` after each applied flush, alongside the existing report publish. `WithHistory(n)` sets how many snapshots beyond the current one to retain; default 0 means only the current snapshot is retained (enough for `PinCurrent`). Each applied flush appends `Snapshot{Version, At, Config, Fields}`.

- [ ] **Step 1: Write the failing tests**

Create `history_test.go`. Cover:
- With no `WithHistory`, `History()` returns exactly one snapshot after the initial load (the current, version 1), newest-first.
- After a source change is applied (drive it with `mamoritest` from the just-built kit, which is available), `History()` with `WithHistory(0)` still returns only the current (version 2); with `WithHistory(3)` returns the current plus up to 3 prior, newest-first, versions descending.
- History is bounded: with `WithHistory(2)`, after several changes, `len(History()) <= 3` (2 prior + current).
- Each `Snapshot.Fields` holds the diff that produced it, and `Snapshot.Config` is the config at that version.

Use `mamoritest.NewProvider` + `mamoritest.WaitForSnapshot` (from workstream C) to drive changes deterministically. Import `github.com/xavidop/mamori/mamoritest` in the test.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run TestHistory -v
```

- [ ] **Step 3: Implement `history.go`**

```go
package mamori

import "time"

// Snapshot is one validated configuration the Watcher applied, retained when
// WithHistory is enabled. Config is a full copy of T at that version, so
// enabling history extends the in-memory lifetime of any secret material it
// holds; that is why history defaults to off.
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot
}

// History returns the retained snapshots, newest first, always including the
// current one. With WithHistory(0) (the default) only the current snapshot is
// returned. It is lock-free.
func (w *Watcher[T]) History() []Snapshot[T] {
	p := w.snapshots.Load()
	if p == nil {
		return nil
	}
	src := *p
	out := make([]Snapshot[T], len(src))
	copy(out, src)
	return out
}
```

Add `WithHistory` to `reconcile.go`:

```go
// WithHistory retains the n most recent snapshots in addition to the current
// one, readable via Watcher.History and pinnable via Watcher.Pin. It defaults
// to 0.
//
// Retained snapshots hold full copies of T, including any secret material that
// has since been rotated. Enabling history extends the in-memory lifetime of
// old secrets; enable it deliberately.
func WithHistory(n int) Option {
	return func(o *options) {
		if n < 0 {
			n = 0
		}
		o.historyN = n
	}
}
```

Add `historyN int` to the `options` struct.

Add `ErrNoSuchSnapshot` to `errors.go`:

```go
// ErrNoSuchSnapshot is returned by Watcher.Pin when the requested version is not
// retained. Increase WithHistory to pin older versions.
var ErrNoSuchSnapshot = errors.New("mamori: no such snapshot version")
```

- [ ] **Step 4: Wire history into the engine**

Add to `Watcher[T]`: `snapshots atomic.Pointer[[]Snapshot[T]]`.

Add to `engine[T]`: `history []Snapshot[T]` and read `o.historyN` for the bound.

In `Watch`, after building the engine and before `e.start`, seed the initial snapshot and publish:

```go
	e.history = []Snapshot[T]{{Version: e.version, At: now, Config: cfg}}
	w.snapshots.Store(&e.history)
```

Add a helper the flush path calls when it applies a snapshot:

```go
// recordSnapshot appends the applied config to the bounded history and
// republishes it. Called only by the reconciler goroutine.
func (e *engine[T]) recordSnapshot(cfg T, fields []FieldChange) {
	snap := Snapshot[T]{Version: e.version, At: e.o.clock.Now(), Config: cfg, Fields: fields}
	e.history = append(e.history, snap)
	// Retain historyN prior snapshots plus the current one.
	if max := e.o.historyN + 1; len(e.history) > max {
		e.history = e.history[len(e.history)-max:]
	}
	published := make([]Snapshot[T], len(e.history))
	for i, s := range e.history {
		published[len(e.history)-1-i] = s // newest first
	}
	e.w.snapshots.Store(&published)
}
```

In `flush`, after `e.version++` and the config apply, call `e.recordSnapshot(cand, fields)`. (Task 2 will make flush pin-aware; for now, record on every applied flush.)

**A slicing subtlety to get right:** `e.history[len(e.history)-max:]` re-slices the same backing array, so an earlier published pointer could observe later mutation. Since `recordSnapshot` copies into a fresh `published` slice before storing, the published pointer is immutable; confirm the internal `e.history` reslice does not corrupt an already-published copy (it does not, because `published` is a copy). Note this in your report.

- [ ] **Step 5: Run, race, full suite**

```bash
GOWORK=off go test ./... -run TestHistory -v
GOWORK=off go test -race ./...
make test
```

- [ ] **Step 6: Stage**

```bash
git add history.go history_test.go reconcile.go reconciler.go errors.go
```

```
feat(core): add bounded snapshot history

The engine now retains recent validated snapshots and publishes them through
an atomic pointer, readable lock-free via Watcher.History. History defaults to
off because retained snapshots hold full config copies including rotated
secrets; WithHistory(n) opts in with that tradeoff documented.
```

---

### Task 2: Pin, PinCurrent, Unpin

**Files:**
- Create: `pin.go`
- Modify: `reconciler.go` (control channel in the loop; pin-aware flush; `Watcher` fields; `Close`)
- Modify: `report.go` (set `Report.Pinned`)
- Create: `pin_test.go`

**Interfaces:**
- Consumes: `History`, `ErrNoSuchSnapshot` (Task 1); the version counter, `Change[T]`, the flush machinery.
- Produces: `Watcher.Pin(version uint64) error`, `Watcher.PinCurrent() uint64`, `Watcher.Unpin()`, `Watcher.Pinned() (uint64, bool)`, and `Report.Pinned`.

**The design, and why it is a control channel and not an atomic flag.** Unpin must apply the live config to `Get()` AND emit exactly one coalesced `Change`, and that must happen on the reconciler goroutine to preserve serial `OnChange` delivery. Pin must look up a config from history and freeze `Get()` at it. Doing these from a caller goroutine with an atomic flag would race the reconciler's own `cfg.Store` and `enqueue`. So Pin/PinCurrent/Unpin send a command through a control channel that the reconciler `loop` selects on, and wait for a reply. Reads (`Pinned`, `Status`, `History`) stay lock-free via published pointers.

**Pin-aware flush semantics (spec 9.2):** while pinned, `flush` still builds, decodes, and validates the candidate, still advances the version (Live), and still records the snapshot in history, but does NOT call `cfg.Store` and does NOT enqueue a `Change`. So `Get()` keeps returning the pinned config, watches stay live, `Status()` shows `Snapshot` at the pinned version and `Live` at the newest, and a validation failure while pinned still reaches `OnError`. `Unpin()` applies the newest validated config and emits one `Change` whose `Fields` is the accumulated diff between the pinned and live snapshots.

- [ ] **Step 1: Write the failing tests**

Create `pin_test.go`. Use `mamoritest`. Cover:
- `PinCurrent()` returns the current version; after it, `Set`ting a new value and `WaitForSnapshot` on the LIVE version (via `Status().Live`) advances Live while `Get()` stays at the pinned value and `Status().Snapshot` stays at the pinned version and `Status().Pinned` is true.
- `Unpin()` then makes `Get()` return the latest value, `Status().Pinned` false, `Snapshot == Live`, and fires exactly one `OnChange` whose `Fields` names the field(s) that changed while pinned. Assert the single-event coalescing: multiple `Set`s while pinned produce ONE `Change` on unpin, not one per set.
- `Pin(v)` for a retained older version (with `WithHistory(3)`) freezes `Get()` at that version's config; `Pin(v)` for a non-retained version returns `ErrNoSuchSnapshot`.
- `Pinned()` returns `(version, true)` while pinned, `(0, false)` otherwise.
- A validation failure while pinned still reaches `OnError` (drive with `mamoritest` + a `validate` tag and an invalid value; use `CaptureErrors`).
- `-race`: `Pin`/`Unpin`/`Status`/`Get` called concurrently with reconciliation is clean.
- Pinning then `Close()` does not deadlock or leak (goleak).

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run TestPin -v
```

- [ ] **Step 3: Add the control channel and Watcher fields**

Add to `Watcher[T]`:

```go
	control chan pinCmd
	closed  chan struct{} // closed by Close so Pin/Unpin do not block after shutdown
```

Initialize both in `Watch` (`control: make(chan pinCmd)`, `closed: make(chan struct{})`).

In `pin.go`:

```go
package mamori

type pinKind int

const (
	pinAt pinKind = iota
	pinCurrent
	unpin
)

type pinCmd struct {
	kind    pinKind
	version uint64
	reply   chan pinReply
}

type pinReply struct {
	version uint64
	err     error
}

// send delivers a command to the reconciler and waits for its reply, or returns
// an error if the watcher is closing.
func (w *Watcher[T]) sendPin(cmd pinCmd) pinReply {
	cmd.reply = make(chan pinReply, 1)
	select {
	case w.control <- cmd:
		return <-cmd.reply
	case <-w.closed:
		return pinReply{err: errWatcherClosed}
	}
}
```

Add `errWatcherClosed` to `errors.go` (`var errWatcherClosed = errors.New("mamori: watcher closed")`), unexported (callers see it via Pin's returned error).

Implement the public methods:

```go
// Pin freezes Get at the snapshot with the given version and stops applying
// reconciled updates to Get, though sources keep being watched and Status keeps
// showing the diverging live version. Returns ErrNoSuchSnapshot if that version
// is not retained (raise WithHistory to pin older versions).
func (w *Watcher[T]) Pin(version uint64) error {
	return w.sendPin(pinCmd{kind: pinAt, version: version}).err
}

// PinCurrent freezes Get at whatever snapshot it returns now and returns that
// version. It always succeeds and needs no retained history.
func (w *Watcher[T]) PinCurrent() uint64 {
	return w.sendPin(pinCmd{kind: pinCurrent}).version
}

// Unpin resumes applying reconciled updates, applies the newest validated
// snapshot to Get, and delivers one coalesced Change for everything that
// changed while pinned. It is a no-op if not pinned.
func (w *Watcher[T]) Unpin() {
	w.sendPin(pinCmd{kind: unpin})
}

// Pinned reports the pinned version and whether the watcher is currently pinned.
func (w *Watcher[T]) Pinned() (uint64, bool) {
	rep := w.Status()
	return rep.Snapshot, rep.Pinned && rep.Snapshot != rep.Live || rep.Pinned
}
```

**Fix `Pinned()`'s logic** rather than shipping that muddled boolean: `Pinned()` should return `(pinnedVersion, true)` when pinned and `(0, false)` otherwise. Read the pin state from the published report; add a dedicated `pinnedVersion` to the report or a separate atomic so `Pinned()` is unambiguous. Report the exact mechanism you chose (a `Report.Pinned bool` plus using `Report.Snapshot` as the pinned version when `Pinned` is true is the natural choice, since `Snapshot` IS the served/pinned version while pinned).

- [ ] **Step 4: Handle commands in the reconciler loop, and make flush pin-aware**

Add engine pin state (reconciler-owned): `pinned bool`, `pinnedVersion uint64`, `pinnedConfig T`, `pinnedApplied map[string]string` (a copy of `applied` captured at pin time, for the unpin diff).

Add a `case cmd := <-e.controlCh:` to the loop's `select` (wire `e.controlCh = w.control`). Handle:

```go
func (e *engine[T]) handlePin(cmd pinCmd) {
	switch cmd.kind {
	case pinCurrent:
		e.pinned = true
		e.pinnedVersion = e.version
		e.pinnedConfig = e.lastGood
		e.pinnedApplied = copyStringMap(e.applied)
		cmd.reply <- pinReply{version: e.version}
	case pinAt:
		snap, ok := e.findSnapshot(cmd.version)
		if !ok {
			cmd.reply <- pinReply{err: ErrNoSuchSnapshot}
			return
		}
		e.pinned = true
		e.pinnedVersion = cmd.version
		e.pinnedConfig = snap.Config
		e.pinnedApplied = copyStringMap(e.applied)
		e.w.cfg.Store(&snap.Config)
		cmd.reply <- pinReply{version: cmd.version}
	case unpin:
		if !e.pinned {
			cmd.reply <- pinReply{}
			return
		}
		e.pinned = false
		old := e.pinnedConfig
		newCfg := e.lastGood
		e.w.cfg.Store(&newCfg)
		fields := e.diffApplied(e.pinnedApplied, e.applied)
		if len(fields) > 0 {
			e.enqueue(Change[T]{Old: old, New: newCfg, Fields: fields})
		}
		e.pinnedApplied = nil
		cmd.reply <- pinReply{}
	}
	e.w.report.Store(e.buildReport())
}
```

`findSnapshot` searches `e.history` (including the current) for the version. `copyStringMap` and `diffApplied` are small helpers; `diffApplied(before, after)` returns a `FieldChange` for each path whose version differs.

Make `flush` pin-aware: extract the build+validate+diff into a helper returning `(cand T, fields []FieldChange, ok bool)`. Then:
- `ok == false` (nothing to apply): return.
- validation failed: `emitErr(&ValidationError{...})` and return (this path must run whether pinned or not, so `OnError` fires while pinned).
- pinned: `e.version++`, `e.lastGood = cand`, advance `e.applied` (already done in the diff step), `e.recordSnapshot(cand, fields)`, publish report. Do NOT `cfg.Store`, do NOT enqueue.
- not pinned: `e.version++`, `e.w.cfg.Store(&cand)`, `e.lastGood = cand`, `recordSnapshot`, `RecordRefresh`, `enqueue(Change)`, publish report (the existing behavior).

**Close must not deadlock Pin.** In `Close`, `close(w.closed)` FIRST (so any in-flight `sendPin` unblocks with `errWatcherClosed`), then `cancel()`, then `wg.Wait()`. Confirm the loop's `controlCh` select does not block shutdown: the loop exits on `ctx.Done()` regardless of pending control commands, and `sendPin` falls back to `<-w.closed`.

- [ ] **Step 5: Set Report.Pinned**

In `report.go`'s `buildReport`, set `Pinned: e.pinned`, `Snapshot: servedVersion`, `Live: e.version`, where `servedVersion` is `e.pinnedVersion` when pinned and `e.version` otherwise. Confirm `Status()`'s read-time recomputation does not clobber `Pinned`/`Snapshot`/`Live` (it only recomputes Age/Stale/Healthy).

- [ ] **Step 6: Run, race, full suite, goleak**

```bash
GOWORK=off go test ./... -run 'TestPin|TestHistory|TestStatus' -v
GOWORK=off go test -race ./...
make test
```

Prove the two hardest properties explicitly and report them: (a) multiple `Set`s while pinned produce exactly ONE coalesced `Change` on `Unpin`, with the correct accumulated `Fields`; (b) a validation failure while pinned reaches `OnError` while `Get()` stays at the pinned value.

- [ ] **Step 7: Stage**

```bash
git add pin.go pin_test.go reconciler.go report.go errors.go
```

```
feat(core): add Pin, PinCurrent, and Unpin

An operator can freeze Get at a known-good snapshot while sources keep being
watched, then resume with a single coalesced Change for everything that
changed while pinned. Pin/Unpin route through a control channel into the
reconciler goroutine so config application and OnChange delivery stay serial;
reads stay lock-free. Validation failures while pinned still reach OnError.
```

---

### Task 3: Documentation

**Files:**
- Modify: `site/src/pages/docs/observability.md` or `usage.md` (Pin/Unpin/History in the watch walkthrough)
- Modify: `site/src/pages/docs/concepts.md` (snapshot versioning and pinning)
- Modify: `site/src/pages/docs/security.md` (history secret-retention tradeoff)
- Modify: `README.md`

**Interfaces:** consumes Tasks 1-2.

- [ ] **Step 1: Document it**

Cover: snapshot versioning (1 at load, +1 per applied change), `History()` and the `WithHistory` retention tradeoff (retained snapshots hold rotated secrets, default off), and the Pin/Unpin operational pattern ("freeze config, I am debugging prod"): pin, investigate, unpin. Show that while pinned `Status()` shows `Snapshot` vs `Live` divergence, and that Unpin delivers one coalesced `Change`. State that `Pin`/`Unpin` are not exposed over the admin HTTP endpoint (they change behavior; an app wanting remote pinning should mount its own authenticated route calling `w.Pin`). Verify examples compile against the real signatures.

Add the history secret-retention tradeoff to `security.md`.

- [ ] **Step 2: Build, stage**

```bash
make site-build   # Node 22; nvm use 22 if the engine check fails
git add site/src/pages/docs/ README.md
```

```
docs: document snapshot history and Pin/Unpin

Adds the freeze-config-while-debugging pattern, the Snapshot/Live divergence
shown under Status while pinned, and the WithHistory secret-retention
tradeoff. Notes that Pin/Unpin are not exposed over the admin HTTP endpoint.
```

---

## Self-Review

**Spec coverage.** Implements spec section 9 in full: 9.1 versioning (already present, consumed here), the `Snapshot`/`WithHistory`/`History` retention model (Task 1), and 9.2 Pin/PinCurrent/Unpin with the pin-aware flush and single coalesced unpin `Change` (Task 2). `Report.Pinned` completes the report shape from workstream B.

**Placeholders.** None, with one deliberate correction called out: the draft `Pinned()` body in Task 2 Step 3 is shown as muddled ON PURPOSE with an immediate instruction to implement it cleanly (return `(pinnedVersion, true)` / `(0, false)` from the published report), so the implementer does not transcribe a bad boolean. Every other code block is complete.

**Type consistency.** `Snapshot[T]` is defined once and used by `History`, `recordSnapshot`, and the pin lookup. The pin control flow uses one `pinCmd`/`pinReply` pair. `flush` is refactored so the build+validate+diff logic is shared between the pinned and unpinned paths, so "what counts as a change" cannot diverge between them. `Report.Pinned`/`Snapshot`/`Live` are set in one place (`buildReport`) and only Age/Stale/Healthy are recomputed at read time.

**Risk noted.** Two areas carry the most risk. First, the flush refactor: it must preserve the exact existing unpinned behavior (validation-reject keeps last-good, empty-diff is a no-op, version increments only on a non-empty applied diff), so the plan extracts a shared helper rather than duplicating logic, and Task 2 re-runs the workstream-B version-sequence and validation tests. Second, the control-channel shutdown ordering: `Close` must `close(w.closed)` before `cancel()` so an in-flight `Pin` cannot block forever, and the loop must exit on `ctx.Done()` regardless of pending commands; the plan mandates a pin-then-Close goleak test. The secret-retention tradeoff of `WithHistory` is documented in both the option doc comment and `security.md`.
