---
layout: ../../layouts/DocsLayout.astro
title: Loading & watching
---

# Loading & watching

Two entry points, both generic over your config type `T`: `Load` for a one-shot read, `Watch` to stay reconciled.

## Loading

`Load` resolves every ref once, applies defaults, validates, and returns the typed config. It fails fast: on any resolve or validation error it returns the zero value and a non-nil error. Partial config is never returned.

```go
cfg, err := mamori.Load[Config](ctx, opts...)
```

Batch-capable providers (for example AWS Secrets Manager) are resolved in a single API call automatically.

## Watching

`Watch` performs the same fail-fast initial load, then keeps the config reconciled and hands you diff-aware callbacks. It returns after the initial config is resolved; `OnChange` fires only on *subsequent* changes.

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithPollInterval(30*time.Second),
	mamori.OnChange(func(ev mamori.Change[Config]) {
		if ev.Changed("DBPassword") {
			pool.Rotate(ev.New.DBPassword.Reveal())
		}
		for _, f := range ev.Fields {
			log.Printf("%s changed: %s -> %s", f.Path, f.OldVersion, f.NewVersion)
		}
	}),
	mamori.OnError(func(err error) { metrics.Inc("config_error") }),
)
if err != nil {
	log.Fatal(err)
}
defer w.Close()

cfg := w.Get() // lock-free atomic snapshot; always the last valid config

w.Status() // per-field health report: refs, staleness, last error kind
```

`Change[T]` carries `Old` and `New` full snapshots plus `Fields []FieldChange{Path, OldVersion, NewVersion}`, and a helper `Changed(path string) bool`. See [Observability](../observability) for `Status`, `Health`, and the pre-deploy `Doctor` check.

## Watch semantics

These are guaranteed and covered by the conformance kit:

- **Atomicity.** `OnChange` fires with a fully re-validated snapshot. If a new value fails validation the update is rejected: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`. The config never transitions to a broken state mid-flight.
- **Coalescing.** Field changes within a debounce window (default 500ms; override per field with `?debounce=`) produce a single `Change` event. A JSON secret with five keys rotating is one event, not five.
- **Ordering.** `OnChange` callbacks are serialized on one goroutine. A slow callback delays but never drops the next event; a bounded queue with a drop-oldest policy guards against a pathological consumer (`WithQueueDepth`).
- **First event.** `Watch` resolves the initial config before returning (fail-fast on startup). `OnChange` fires only on later changes.
- **Shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.

On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref is retried with per-ref exponential backoff. `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.

## Source chains

A `source` tag can hold a comma-separated **precedence chain** of refs instead of a single one: the first ref that resolves to a value wins, a not-found falls through to the next ref, and any other error stops the walk there and is handled by the field's `onfail` policy. See [Concepts](../concepts#source-chains-and-precedence) for the full grammar, the comma-ambiguity rule, and the `onfail` table - this section works through three concrete cases.

### A precedence chain with a default

Prefer an explicit environment override, fall back to a centrally managed Parameter Store value, and fall back again to a default if neither is set:

```go
type Config struct {
	Port string `source:"env:PORT,aws-ps://svc/port" default:"8080"`
}

cfg, err := mamori.Load[Config](ctx)
```

- `PORT` set in the environment: that value wins; `aws-ps://svc/port` is never even resolved.
- `PORT` unset, `aws-ps://svc/port` present: the Parameter Store value wins.
- Neither set (both refs resolve not-found): `Port` is `"8080"`, the `default:`.
- `PORT` unset and `aws-ps://svc/port` returns a permission-denied error: `Load` **fails**. A real error never falls through to `default:` on its own; see `onfail` below for the explicit opt-in.

### Failing loudly on a chain error: `onfail:"fail"`

By default (`onfail:"keeplast"`), a runtime chain error on `Watch` keeps the last-known-good value and reports the error via `OnError`; on the very first `Load` there is no last-known-good value, so it fails. If instead you want the candidate rejected on *every* non-not-found error, tag the field `onfail:"fail"`:

```go
type Config struct {
	APIKey secret.String `source:"aws-sm://prod/api-key,file:///etc/apikey.txt" onfail:"fail"`
	Other  string        `source:"env:OTHER"`
}
```

If `aws-sm://prod/api-key` starts returning a permission-denied error at runtime, `onfail:"fail"` rejects the *whole* candidate update, not just `APIKey`: even an unrelated, otherwise-valid change to `Other` arriving in the same reconcile is held back until the chain error clears. `Get()` keeps serving the last snapshot that fully succeeded, and `OnError` still receives the `*ProviderError`. Once `aws-sm://prod/api-key` resolves again, everything held back applies together in one `Change`.

### Live precedence: takeover and fallback

Every position in a chain is watched, not just the current winner, so precedence is live:

```go
type Config struct {
	Port string `source:"chain-a://port,chain-b://port"`
}

w, err := mamori.Watch[Config](ctx /* , providers for chain-a and chain-b */)
// chain-a starts absent, chain-b holds "8080": w.Get().Port == "8080"

// chain-a is exported at runtime:
//   -> Watch recomputes the winner and delivers one Change (Old: "8080", New: "9090")
//   -> w.Get().Port == "9090"

// chain-a is unset again:
//   -> falls back to chain-b's last known value, one more Change (Old: "9090", New: "8080")
//   -> w.Get().Port == "8080"
```

A change to a position that is not currently winning (`chain-b` updating while `chain-a` is present) is still observed - it shows up in `w.Status()` - but it does not move `Get()` or fire a `Change`, since a higher-priority ref is already winning.

## Snapshot history and pinning

Every `Watcher` tracks a monotonically increasing **snapshot version**: `1` at the initial load inside `Watch`, then `+1` each time a validated, non-empty update is applied. That counter keeps climbing in the background even while `Get()` is frozen by a pin (below), since sources are still watched and reconciled the whole time - a pin only holds back what reaches `Get()` and `OnChange`.

```go
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot
}
```

### Retaining snapshots with `WithHistory`

By default a `Watcher` keeps only its current snapshot. `WithHistory(n)` retains the `n` most recent snapshots in addition to the current one:

```go
func WithHistory(n int) Option
func (w *Watcher[T]) History() []Snapshot[T] // newest first, always includes current
```

```go
w, err := mamori.Watch[Config](ctx, mamori.WithHistory(10))
if err != nil {
	log.Fatal(err)
}
defer w.Close()

for _, snap := range w.History() {
	log.Printf("v%d at %s: %d field(s) changed", snap.Version, snap.At, len(snap.Fields))
}
```

Retained snapshots hold a **full copy of `T`**, including any `secret.String` / `secret.Bytes` field's value at that version - even after the live config has since rotated that field away. Enabling history extends how long old secret material stays reachable in process memory, which is exactly why `WithHistory` defaults to `0` (off). Read [Security](../security#withhistory-retains-past-secrets-in-memory) before enabling it in a service that handles credentials, and size `n` to the operational need you actually have.

### Freezing `Get()` while you debug: `Pin` / `PinCurrent` / `Unpin`

Sometimes you want the config to hold still while you investigate something in production, without stopping the watcher itself: sources should keep being watched and reconciled, but whatever `Get()` returns should not shift under you mid-investigation.

```go
func (w *Watcher[T]) Pin(version uint64) error // ErrNoSuchSnapshot if version is not retained
func (w *Watcher[T]) PinCurrent() uint64       // pins at whatever Get() serves right now; always succeeds
func (w *Watcher[T]) Unpin()                   // resumes; a no-op if not currently pinned
func (w *Watcher[T]) Pinned() (uint64, bool)   // the current pin, if any
```

The pin, investigate, unpin pattern:

```go
w, err := mamori.Watch[Config](ctx, mamori.WithHistory(10))
if err != nil {
	log.Fatal(err)
}
defer w.Close()

// Something looks wrong. Freeze config before poking around.
version := w.PinCurrent()
log.Printf("pinned at snapshot %d", version)

rep := w.Status()
log.Printf("snapshot=%d live=%d pinned=%v", rep.Snapshot, rep.Live, rep.Pinned)
// Sources are still watched, so Live can keep advancing while Snapshot holds
// still: rep.Snapshot is what Get() returns, rep.Live is the newest validated
// version underneath it. A validation failure on a live update still reaches
// OnError even while pinned; it just never becomes what Get() serves.

// ... investigate; Get() keeps returning exactly the pinned config ...

w.Unpin()
// Get() resumes tracking the newest validated snapshot. OnChange fires
// exactly one Change, whose Fields is the accumulated diff of everything
// that changed while pinned, however many updates landed in the meantime.
// If nothing changed while pinned, Unpin fires no Change at all.
```

To go back further than the currently pinned snapshot, `Pin` a specific retained version by number. It fails if that version has fallen outside the `WithHistory` window:

```go
if err := w.Pin(42); err != nil {
	if errors.Is(err, mamori.ErrNoSuchSnapshot) {
		// not retained; raise WithHistory(n) to reach further back
	}
}
```

`Pin` and `Unpin` are **not** exposed over the admin HTTP endpoint ([HTTP exposure](../observability#http-exposure)): that endpoint is deliberately read-only metadata, while pinning changes what your application actually does with `Get()`. An app that wants remote pinning should mount its own authenticated route that calls `w.Pin` / `w.Unpin` directly, rather than relying on `Handler` or `WithAdminHTTP` for it.

## Options

All options apply to both `Load` and `Watch` unless noted.

| Option | Purpose |
| --- | --- |
| `WithProvider(p)` | Register a provider for this call, overriding the registry for its scheme. |
| `WithExecProvider()` | Enable the opt-in `exec:` provider for this call. |
| `WithValidator(v)` | Replace the default go-playground/validator. |
| `WithDecodeHook(h)` | Add a mapstructure decode hook (flatten path). |
| `WithClock(c)` | Swap the clock (deterministic tests). |
| `WithPollInterval(d)` | Fallback poll interval for non-watchable providers (default 30s). |
| `WithJitter(f)` | Poll jitter fraction 0..1 (default 0.2). |
| `WithDebounce(d)` | Coalescing window (default 500ms). |
| `WithQueueDepth(n)` | `OnChange` dispatch queue depth, drop-oldest when full (default 16). |
| `WithBackoff(base, max)` | Per-ref exponential backoff bounds on resolve failure. |
| `WithStale(maxAge)` | Escalate staleness to a hard error. |
| `WithHistory(n)` | Retain `n` snapshots beyond the current one, readable via `History()` and pinnable via `Pin` (default 0, off). |
| `WithMeter(m)` / `WithTracer(t)` | OpenTelemetry-style instrumentation (see the OpenTelemetry page). |
| `OnChange(fn)` / `OnError(fn)` | Watch callbacks. |
