---
layout: ../../layouts/DocsLayout.astro
title: Loading & watching
---

# Loading & watching

mamori loads your typed config from refs (environment variables, secret managers, files, and more) and can keep it reconciled as those sources change. Two entry points, both generic over your config type `T`: `Load` reads once, `Watch` stays reconciled and hands you diff-aware callbacks.

## Quick start

Define a config struct, tag each field with a `source:`, and `Load` it:

```go
import (
	"context"
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

type Config struct {
	Port       string        `source:"env:PORT" default:"8080"`
	DBPassword secret.String `source:"aws-sm://prod/db-password"`
}

func main() {
	ctx := context.Background()

	cfg, err := mamori.Load[Config](ctx)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on :%s", cfg.Port)
	pool.Connect(cfg.DBPassword.Reveal())
}
```

That is the whole flow: one call resolves every ref, applies defaults, validates, and returns a fully typed `Config`.

## Load config once

`Load` resolves every ref once, applies defaults, validates, and returns the typed config. It fails fast: on any resolve or validation error it returns the zero value and a non-nil error, so you never get partial config.

```go
cfg, err := mamori.Load[Config](ctx, opts...)
```

Batch-capable providers (for example AWS Secrets Manager) are resolved in a single API call automatically.

## Watch for changes

`Watch` does the same fail-fast initial load, then keeps the config reconciled in the background. It returns once the initial config is resolved; `OnChange` fires only on later changes. Read the current config any time with `Get()`.

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
```

`Change[T]` carries `Old` and `New` full snapshots plus `Fields []FieldChange{Path, OldVersion, NewVersion}`, and a `Changed(path string) bool` helper for reacting to one field.

### What you can rely on

These behaviors are guaranteed and covered by the conformance kit. (The mechanism behind them is in [How it works](#how-it-works).)

- **Validated, all-or-nothing updates.** `OnChange` fires with a fully re-validated snapshot. If a new value fails validation the update is rejected: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`. The config never transitions to a broken state mid-flight.
- **OnChange is called one at a time.** Callbacks are serialized, so your callback never runs concurrently with itself. A slow callback delays the next event but never drops it in normal operation.
- **Coalesced events.** Field changes within a debounce window (default 500ms, override per field with `?debounce=`) produce a single `Change`. A JSON secret with five keys rotating is one event, not five.
- **Last-good on failure.** On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref is retried with per-ref exponential backoff. `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.
- **Clean shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.

## Source chains

A `source` tag can hold a comma-separated precedence chain of refs instead of a single one: the first ref that resolves to a value wins, a not-found falls through to the next ref, and any other error stops the walk there and is handled by the field's `onfail` policy.

```go
type Config struct {
	APIKey secret.String `source:"env:API_KEY,aws-sm://prod/api-key"`
}
```

`env:API_KEY` wins when it is set; otherwise mamori reads `aws-sm://prod/api-key`. See [Concepts](../concepts#source-chains-and-precedence) for the full grammar and the comma-ambiguity rule. The three cases below work through the common patterns.

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
- `PORT` unset and `aws-ps://svc/port` returns a permission-denied error: `Load` **fails**. A real error never falls through to `default:` on its own (see `onfail` below for the explicit opt-in).

### Choosing an onfail policy

`onfail` controls what a field does when a chain stops on a genuine error, as opposed to absence. The rule to hold onto: `default:` applies only to genuine absence, never to an error, unless a field explicitly opts in.

| `onfail` value | Behavior |
| --- | --- |
| `keeplast` (default) | Keep the last successfully applied value and report the error via `OnError` / `Status`. On the very first `Load` there is no prior value to keep, so it fails. |
| `default` | Apply the field's `default:` value, as if every ref had resolved not-found. The explicit opt-in for "treat this error like absence." Requires a `default:` tag. |
| `fail` | Reject the candidate outright: the whole update on `Watch` (not just this field), or the whole `Load`. |

Tag a field `onfail:"fail"` when a non-not-found error must reject the whole candidate update:

```go
type Config struct {
	APIKey secret.String `source:"aws-sm://prod/api-key,file:///etc/apikey.txt" onfail:"fail"`
	Other  string        `source:"env:OTHER"`
}
```

If `aws-sm://prod/api-key` starts returning a permission-denied error at runtime, `onfail:"fail"` rejects the whole candidate update, not just `APIKey`: even an unrelated, otherwise-valid change to `Other` arriving in the same reconcile is held back until the chain error clears. `Get()` keeps serving the last snapshot that fully succeeded, and `OnError` still receives the `*ProviderError`. Once `aws-sm://prod/api-key` resolves again, everything held back applies together in one `Change`.

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

A change to a position that is not currently winning (`chain-b` updating while `chain-a` is present) is still observed (it shows up in `w.Status()`) but does not move `Get()` or fire a `Change`, since a higher-priority ref is already winning.

## Check field health with Status

`w.Status()` returns a point-in-time `Report` of every field's health: which ref is winning, how stale it is, and the last error kind. It is lock-free and safe to call from any goroutine.

```go
rep := w.Status()
log.Printf("snapshot=%d live=%d pinned=%v healthy=%v",
	rep.Snapshot, rep.Live, rep.Pinned, rep.Healthy)

for _, f := range rep.Fields {
	log.Printf("%s via %s: age=%s stale=%v %s",
		f.Path, f.Ref, f.Age, f.Stale, f.LastError)
}
```

`rep.Snapshot` is the version `Get()` currently returns and `rep.Live` is the newest validated version underneath it; the two diverge only while pinned (below). For a readiness probe, `w.Health()` returns `nil` when every field is fresh and error-free, or a `*HealthError` naming the broken fields. See [Observability](../observability) for `Status`, `Health`, the read-only HTTP exposure, and the pre-deploy `Doctor` check.

## Snapshot history and pinning

A `Watcher` can retain past snapshots and freeze what `Get()` returns while you investigate, without stopping reconciliation underneath: sources keep being watched, only what reaches `Get()` and `OnChange` is held back.

Each applied update is a `Snapshot[T]`:

```go
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot
}
```

`Version` increments on each validated, non-empty update (see [How it works](#how-it-works) for the counter mechanics).

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

Retained snapshots hold a full copy of `T`, including any `secret.String` / `secret.Bytes` field's value at that version, even after the live config has since rotated that field away. Enabling history extends how long old secret material stays reachable in process memory, which is exactly why `WithHistory` defaults to `0` (off). Read [Security](../security#withhistory-retains-past-secrets-in-memory) before enabling it in a service that handles credentials, and size `n` to the operational need you actually have.

### Pin and unpin during a rollout

During a rollout you often want the config to hold still while you investigate, without stopping the watcher: sources should keep being watched and reconciled, but whatever `Get()` returns should not shift under you mid-investigation.

```go
func (w *Watcher[T]) Pin(version uint64) error // ErrNoSuchSnapshot if version is not retained
func (w *Watcher[T]) PinCurrent() uint64       // pins whatever Get() serves right now; always succeeds
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

`Pin` and `Unpin` are **not** exposed over the admin HTTP endpoint ([HTTP exposure](../observability#serve-the-report-over-http)): that endpoint is deliberately read-only metadata, while pinning changes what your application actually does with `Get()`. An app that wants remote pinning should mount its own authenticated route that calls `w.Pin` / `w.Unpin` directly, rather than relying on `Handler` or `WithAdminHTTP` for it.

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

## How it works

The behaviors above are the contract. Here is the machinery behind them.

- **Snapshot version counter.** Every `Watcher` tracks a monotonically increasing snapshot version: `1` at the initial load inside `Watch`, then `+1` each time a validated, non-empty update is applied. That counter keeps climbing in the background even while `Get()` is frozen by a pin, since sources are still watched and reconciled the whole time. A pin only holds back what reaches `Get()` and `OnChange`, which is the `Live` versus `Snapshot` divergence `Status` reports.
- **Serial OnChange delivery.** Callbacks run on a single dispatch goroutine, so they are strictly serialized: a slow callback delays but never overlaps the next event.
- **Drop-oldest queue.** Dispatch goes through a bounded queue (`WithQueueDepth`, default 16). If a pathological consumer falls far enough behind to fill it, the oldest queued event is dropped rather than blocking the reconciler. Because each `Change` carries full `Old` / `New` snapshots, a dropped intermediate notification never leaves `Get()` wrong, it only skips a notification.
- **Single reconciler goroutine.** One reconciler goroutine owns all engine state and applies every update, pin, and unpin. That is what lets `Unpin` apply the newest config and emit exactly one coalesced `Change` without racing a concurrently landing update.
- **Coalescing window.** Field changes are batched by a debounce timer; a batch touching several fields uses the tightest of their windows, so one burst is still one `Change`.

## See also

- [Concepts](../concepts) for refs, the tag grammar, source chains, and error kinds.
- [Validation](../validation) for the defaults and validation rules applied on every load.
- [Observability](../observability) for `Status`, `Health`, `Doctor`, and the read-only HTTP surface.
- [Security](../security#withhistory-retains-past-secrets-in-memory) for the secret-retention tradeoff of `WithHistory`.
