# mamori rotation safety: pre-apply verification and forced refresh

**Date:** 2026-07-28
**Status:** draft, not yet implemented
**Scope:** core module (`reconcile.go`, `reconciler.go`, `doc.go`), README and docs site

---

## 1. Context

mamori's pitch is that configuration and secrets stay reconciled at runtime: a
value rotates upstream, mamori re-resolves, re-validates, and atomically swaps in
the new snapshot without a restart.

The gap is what "validates" means. Today it means the struct passes its
`validate` tags. A rotated database password that is syntactically a perfectly
good string, and functionally wrong, sails through: `Load` succeeds, the swap
happens, `OnChange` fires *after* the fact, and the application discovers the
problem in its request path.

That is the single most common way credential rotation actually fails in
production, and mamori currently has no answer to it. `OnChange` is a
notification, not a gate; by the time it runs, `Get()` already returns the new
value.

A second, smaller gap: there is no way to make mamori re-resolve *now*.
`Watcher` exposes `Get`, `Status`, `Health`, `History`, `Pin`, `PinCurrent`,
`Pinned`, `Unpin`, `AdminAddr`, and `Close`. An operator who knows a secret just
rotated, or who wants a SIGHUP to mean something, has to wait out the poll
interval.

## 2. Goals

1. Let an application **prove a new configuration works before it becomes
   current**, using the engine's existing reject-and-keep-last-good machinery
   rather than new state.
2. Let an operator force an immediate re-resolve.
3. Document the credential-overlap pattern that already works, so nobody builds
   machinery for it.

## 3. Non-goals

- **No dual-credential machinery.** No `secret.Pair`, no `?stage=` ref option.
  `WithHistory(1)` plus `w.History()` already retains the previous validated
  snapshot, which is what a service validating incoming credentials during a
  rotation overlap needs. The real gap there is documentation, not code. See
  section 7.
- **No retry or backoff policy for a rejected candidate.** A rejection leaves the
  field's existing per-ref backoff and watch behavior untouched, so the next
  upstream change or poll produces a fresh candidate. Adding retry state to the
  reconciler is a separate design with its own timing questions.
- **No mutating route on the admin endpoint.** See D5.
- **No refresh verb in the config server's wire protocol in THIS spec.** See D8.
  Client-to-server re-fetch already works and needs no change. Client-to-upstream
  propagation is wanted and is coming, `Policy`-gated, in its own spec; it is
  out of scope here, not refused.
- **`PreApply` is not a second validator.** Struct validation stays where it is.
  `PreApply` exists for checks that need I/O, which is precisely why it needs a
  timeout and the `Validator` does not.

## 4. Decisions

| ID | Decision | Rationale |
|----|----------|-----------|
| D1 | `PreApply` **rejects a candidate snapshot** by returning an error, exactly as a validation failure does | The engine already has one well-tested outcome for "this candidate is not acceptable": reject it, keep serving the last good config, deliver the reason to `OnError`. Reusing that path means no new state, no new `Get()` semantics, and no new failure mode. An observe-only hook would not solve the problem, since a bad credential still goes live. |
| D2 | It runs **inline on the reconciler goroutine**, not on the `OnChange` dispatch queue | It must complete before the swap, and the dispatch queue is asynchronous and lossy by design (`WithQueueDepth` drops the oldest event when full). A gate cannot be delivered on a channel that is allowed to drop. |
| D3 | It is therefore given a **mandatory bounded timeout**, `WithPreApplyTimeout`, defaulting to 10s | D2's consequence: user code doing network I/O now runs on the goroutine that also services every other field's updates, `Status` publication, and pin/unpin. An unbounded hook would wedge the whole reconciler. The timeout is not optional and cannot be disabled; a caller wanting longer sets it explicitly. |
| D4 | **A timeout is a rejection**, not an acceptance | On timeout mamori does not know whether the new configuration works. Applying it anyway would defeat the feature. Rejecting keeps the last good config, which is the safe direction, and `OnError` receives a distinguishable `*PreApplyError` wrapping `context.DeadlineExceeded`. A hook that always times out therefore wedges *updates* (loudly, once per attempt) rather than serving unverified config: that is the correct trade and is documented. |
| D5 | **No `POST /refresh` on the admin endpoint** | `Handler`'s doc comment states it serves exactly `GET /` and `GET /healthz`, that every other path and method is 404, and that there is no route which serves or changes configuration. That read-only property is a security feature of a surface which reports on secret material. A caller who wants a refresh endpoint can mount one on their own mux calling `w.Refresh`, with their own authorization. |
| D6 | `Refresh` **blocks until the resulting candidate has been applied or rejected**, and returns the rejection reason | A `Refresh` that returned as soon as the request was queued would be untestable and useless in a SIGHUP handler, which wants to know whether the reload worked. |
| D7 | `PreApply` runs on the **initial load** too, and a rejection fails `Watch` | `Watch` is already fail-fast on its initial `Load`. A hook that verifies a credential should verify the first one as well; discovering at startup that the configured credential does not work is strictly better than discovering it on the first rotation. |
| D8 | A client's `Refresh` **re-fetches from the config server. Upstream propagation is a real feature, deliberately deferred to its own spec, NOT refused.** This spec's scope ends at the client. | Re-fetching already works with no change: `providers/mamori`'s `Resolve` does a fresh `GET` per call with no client-side cache, so `Refresh` on a `mamori://` field re-reads the server's current value. That is useful after a dropped stream and is safe, being just another read. Propagating upstream is genuinely wanted (owner's call, 2026-07-28) but must be **`Policy`-gated rather than available to every reader**: the server exists so that N consumers cost one upstream watch rather than N, and an ungated client-triggered refresh inverts that, turning N clients across M bindings into N*M on-demand calls against rate-limited, per-call-billed backends. Authorization, not refusal, is the answer. It touches the wire protocol, the `Policy` surface, and the client, so it is its own spec and follows this one. |

## 5. Feature 1: `PreApply`

### 5.1 API

```go
// PreApply installs a gate that runs before a reconciled snapshot becomes
// current. Returning a non-nil error rejects the candidate: Get keeps returning
// the last valid configuration, OnChange does not fire, and OnError receives a
// *PreApplyError describing the rejection.
//
// It exists for checks that struct validation cannot express because they need
// I/O: that a rotated database password actually opens a connection, that a new
// API token is accepted by its issuer, that a reissued certificate chains to a
// trusted root. Validation answers "is this well-formed"; PreApply answers "does
// this actually work".
func PreApply[T any](fn func(ctx context.Context, ev Change[T]) error) Option

// WithPreApplyTimeout bounds how long a PreApply hook may take. Default 10s.
func WithPreApplyTimeout(d time.Duration) Option
```

Typed to `T` the same way `OnChange` is: stored in `options` as `any` and type-asserted by `Watch[T]`.

```go
w, err := mamori.Watch[Config](ctx,
    mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
        if !ev.Changed("DBPassword") {
            return nil
        }
        return pool.Ping(ctx, ev.New.DBPassword.Reveal())
    }),
    mamori.OnError(func(err error) { metrics.Inc("config_rejected") }),
)
```

### 5.2 Semantics

- Runs **after** struct validation and **before** the atomic swap. Validation is
  cheap and pure; there is no reason to spend a network round trip proving a
  snapshot works when its shape is already wrong.
- `ev.Old` is the currently-serving snapshot; `ev.New` is the candidate.
  `ev.Changed(path)` works exactly as in `OnChange`, which is what makes the
  common "only verify when the credential actually rotated" guard a one-liner.
- The `ctx` is derived from the watcher's context and carries the
  `WithPreApplyTimeout` deadline. It is cancelled on `Close`, so a hook blocked
  on a hanging backend cannot delay shutdown past the timeout.
- A rejection delivers `*PreApplyError{Err: ...}` to `OnError` and leaves
  `Get()`, `Status()`, and history untouched. The candidate is discarded; the
  engine's per-ref state is not rewound, so the next change or poll produces a
  fresh candidate normally.
- On the initial load inside `Watch`, a rejection makes `Watch` return the
  `*PreApplyError` and no watcher (D7). `Load` runs it too, with the same result.
- **While pinned**, `PreApply` still runs. `Pin` freezes what `Get` returns; it
  does not stop the engine from advancing `Live`. Skipping verification while
  pinned would mean `Unpin` applies a snapshot nothing ever gated.

### 5.3 The reentrancy hazard, stated plainly

`PreApply` runs on the reconciler goroutine. A hook that calls back into the
same `Watcher` will deadlock. `w.Get()` is lock-free and safe, but `w.Refresh`,
`w.Pin`, and `w.Unpin` are not, since they are serviced by the very goroutine
the hook is occupying.

This gets a prominent doc comment and a test asserting the timeout fires rather
than hanging forever, so the failure is bounded and diagnosable rather than a
silent wedge.

## 6. Feature 2: `Refresh`

```go
// Refresh forces an immediate re-resolve of every field, bypassing poll
// intervals and per-ref backoff, and blocks until the resulting snapshot has
// been applied or rejected.
//
// It returns nil when a snapshot was applied or when nothing changed, and the
// rejection reason when the candidate failed validation or PreApply. Use it
// from a SIGHUP handler, or after an out-of-band signal that a secret rotated.
func (w *Watcher[T]) Refresh(ctx context.Context) error
```

**Reuse the existing control-channel machinery; do not invent a second one.**
`pin.go` already has exactly this shape: `sendPin` (`pin.go:61`) puts a command
on `w.control`, waits on a reply channel, and falls back to `errWatcherClosed`
(`errors.go:27`) when `w.ctx` is done first. `Refresh` is another command on
that same channel with the same closed-path behavior. This spec adds no new
sentinel and no second control plane.

- Serializes with normal reconciliation rather than racing it, by construction,
  since the reconciler `loop` services `w.control` and `updates` in the same
  select.
- Concurrent calls are safe. Each returns the outcome of the resolve it
  triggered.
- Honors the passed `ctx`: a cancelled context aborts the wait and returns
  `ctx.Err()`. The re-resolve itself is still bounded by the watcher's lifetime.
- Returns `errWatcherClosed` after `Close`, identically to `Pin` and `Unpin`.
  Note that sentinel is unexported today, so a caller cannot `errors.Is` it.
  That is a pre-existing wart shared with `Pin`; exporting it is a reasonable
  follow-up but is deliberately out of scope here, since doing it as a
  side effect of this feature would change `Pin`'s public contract too.
- Does **not** bypass `PreApply`. A forced refresh is still gated, which is the
  point.

## 7. Feature 3: document the credential-overlap pattern

No code. `WithHistory(1)` already retains the previous validated snapshot, and
`w.History()` returns snapshots newest-first including the current one. That is
what a service validating *incoming* credentials during a rotation overlap
needs:

```go
w, _ := mamori.Watch[Config](ctx, mamori.WithHistory(1))

func accept(presented string) bool {
    for _, s := range w.History() { // current, then previous
        if subtle.ConstantTimeCompare(
            []byte(presented), []byte(s.Config.APIKey.Reveal())) == 1 {
            return true
        }
    }
    return false
}
```

The documentation must state the cost as plainly as the benefit: retained
snapshots hold rotated secrets in memory for as long as they are retained, which
is exactly why `WithHistory` defaults to 0. `WithHistory(1)` is the right
setting for this pattern; a larger window widens the overlap and the exposure
together.

This goes in a new "Credential rotation" section of the docs site, cross-linked
from `usage/snapshots.md` and the `WithHistory` godoc.

## 8. Testing

- `PreApply` rejects: candidate discarded, `Get()` unchanged, `OnChange` did not
  fire, `OnError` received a `*PreApplyError`.
- `PreApply` accepts: swap happens, `OnChange` fires once.
- `PreApply` is not called when no field changed.
- Rejection on the initial load fails `Watch` and leaves no watcher running
  (asserted with `goleak`).
- Timeout fires: a hook that blocks forever produces a `*PreApplyError` wrapping
  `context.DeadlineExceeded` within the configured budget, driven by `FakeClock`
  and its `BlockUntil` handshake so the test is deterministic.
- A hook calling `w.Refresh` from inside itself times out rather than hanging
  forever, pinning the section 5.3 hazard.
- `PreApply` still runs while pinned, and `Unpin` applies only gated snapshots.
- `Refresh` applies a change written between polls, with the poll interval set
  to an hour so a pass cannot come from a tick.
- `Refresh` returns the rejection reason when the candidate fails `PreApply`.
- `Refresh` after `Close` returns `errWatcherClosed`; concurrent `Refresh` calls are
  race-clean.
- Race detector on, `goleak` unchanged.

## 9. Documentation

| File | Change |
|---|---|
| `site/src/pages/docs/usage/rotation.md` | **New page.** The problem, `PreApply` with a worked example, `Refresh`, and the history-based overlap pattern with its memory caveat. |
| `site/src/pages/docs/usage/index.md` | `PreApply` in the option walkthrough. |
| `site/src/pages/docs/usage/watching.md` | Where the gate sits in the reconcile cycle. |
| `site/src/pages/docs/usage/snapshots.md` | Cross-link the overlap pattern from `WithHistory`. |
| `site/src/pages/docs/observability/admin.md` | Why there is no `POST /refresh`, and how to mount your own (D5). |
| `site/src/pages/docs/server/index.md` | What `Refresh` means for a `mamori://` consumer: it re-reads the server, it does not force the server upstream, and why that boundary exists (D8). |
| `README.md` | A rotation-safety bullet; `PreApply` in the `Watch` example. |
| `doc.go` | The reconcile cycle now has a gate; say so. |
| `skills/mamori/SKILL.md` | `PreApply`, `Refresh`, and the overlap pattern. |

## 10. Delivery

Stacked on top of the ref-grammar stack rather than branched from `main`:
`PreApply` lands in `reconciler.go`, which that stack modified in four places,
so an independent branch would conflict by construction.

```
xavier/ref-grammar-interpolation   (#60)
 └─ xavier/rotation-preapply       feat: PreApply gate
     └─ xavier/rotation-refresh    feat: Refresh + rotation docs
```

Two PRs. `PreApply` and `Refresh` are independent of each other, and `PreApply`
is the larger and riskier of the two, so it goes first and alone.
