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
```

`Change[T]` carries `Old` and `New` full snapshots plus `Fields []FieldChange{Path, OldVersion, NewVersion}`, and a helper `Changed(path string) bool`.

## Watch semantics

These are guaranteed and covered by the conformance kit:

- **Atomicity.** `OnChange` fires with a fully re-validated snapshot. If a new value fails validation the update is rejected: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`. The config never transitions to a broken state mid-flight.
- **Coalescing.** Field changes within a debounce window (default 500ms; override per field with `?debounce=`) produce a single `Change` event. A JSON secret with five keys rotating is one event, not five.
- **Ordering.** `OnChange` callbacks are serialized on one goroutine. A slow callback delays but never drops the next event; a bounded queue with a drop-oldest policy guards against a pathological consumer (`WithQueueDepth`).
- **First event.** `Watch` resolves the initial config before returning (fail-fast on startup). `OnChange` fires only on later changes.
- **Shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.

On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref is retried with per-ref exponential backoff. `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.

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
| `WithMeter(m)` / `WithTracer(t)` | OpenTelemetry-style instrumentation (see the OpenTelemetry page). |
| `OnChange(fn)` / `OnError(fn)` | Watch callbacks. |
