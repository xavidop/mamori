---
layout: ../../../layouts/DocsLayout.astro
title: Watch for changes
---

# Watch for changes

`Watch` does the same fail-fast initial load as `Load`, then keeps the config reconciled in the background. It returns once the initial config is resolved; `OnChange` fires only on later changes. Read the current config any time with `Get()`.

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

## Read the current config with Get

`Get()` returns a lock-free atomic snapshot of the last valid config. It is safe to call from any goroutine on every request; there is no need to cache the result.

## React to a field with Change and Changed

`OnChange` receives a `Change[T]` carrying `Old` and `New` full snapshots plus `Fields []FieldChange{Path, OldVersion, NewVersion}`. Use `Changed(path string) bool` to react to one field:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("DBPassword") {
		pool.Rotate(ev.New.DBPassword.Reveal())
	}
})
```

## Handle errors with OnError

`OnError` receives runtime resolve and validation errors without stopping the watcher. Use it for metrics and alerting; `Get()` keeps serving the last good config.

```go
mamori.OnError(func(err error) {
	var verr *mamori.ValidationError
	if errors.As(err, &verr) {
		metrics.Inc("config_validation_error")
		return
	}
	metrics.Inc("config_error")
})
```

## What you can rely on

These behaviors are guaranteed and covered by the conformance kit.

- **Validated, all-or-nothing updates.** `OnChange` fires with a fully re-validated snapshot. If a new value fails validation the update is rejected: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`.
- **OnChange is called one at a time.** Callbacks are serialized, so your callback never runs concurrently with itself. A slow callback delays the next event but never drops it in normal operation.
- **Coalesced events.** Field changes within a debounce window (default 500ms, override per field with `?debounce=`) produce a single `Change`. A JSON secret with five keys rotating is one event, not five.
- **Last-good on failure.** On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref is retried with per-ref exponential backoff. `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.
- **Clean shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.

## Tuning the dispatch queue

`OnChange` dispatch goes through a bounded queue. `WithQueueDepth(n)` sets its depth (default 16); when a slow consumer fills it, the oldest queued event is dropped rather than blocking the reconciler. Because each `Change` carries full `Old` / `New` snapshots, a dropped notification never leaves `Get()` wrong.

## See also

- [Source chains](/docs/concepts/source-chains/) for comma-separated precedence and `onfail`.
- [Snapshots and pinning](/docs/usage/snapshots/) for `Status`, history, and pinning.
- [Observability](/docs/observability/) for `Status`, `Health`, and the HTTP surface.
