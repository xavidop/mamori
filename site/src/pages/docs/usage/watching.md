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
- **Last-good on failure.** On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref keeps being retried - on the poll interval by default, or with per-ref exponential backoff if you opt into it with [`WithBackoff`](#retry-backoff). `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.
- **Clean shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.

## Retry backoff

A polled ref that fails to resolve is retried on the poll interval. `WithBackoff(base, max)` replaces that cadence with per-ref exponential backoff for as long as the ref keeps failing: the first retry is delayed by `base`, each further consecutive failure doubles the delay, and it holds at `max`. Any successful round trip with the backend resets it and the ref returns to the normal poll interval.

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithPollInterval(30*time.Second),
	mamori.WithBackoff(30*time.Second, 5*time.Minute),
)
```

**Backoff is off by default.** Without `WithBackoff` a failing ref is retried on `WithPollInterval`, unchanged. Turn it on deliberately, and note that a `base` below your poll interval makes a *just-failed* backend get hit sooner than a healthy one - usually you want `base` at or above the poll interval, as above.

Three things to know before choosing a window:

- **It does not reach providers with a native watch.** A [push provider](/docs/providers/) - Kubernetes informers, Consul blocking queries, Postgres `LISTEN/NOTIFY`, the `mamori://` SSE client - owns its own stream and its own reconnect cadence. mamori polls nothing on its behalf, so there is no attempt for `WithBackoff` to delay; reconnect behavior is provider-internal and documented on each provider's page. The one exception is a native watch that fails to *start*: mamori falls back to polling for that ref, and backoff governs it from then on. `WithBackoff` is a polling knob, and setting it does not make a push provider back off.
- **A not-found is not a failure.** `ErrNotFound` means the backend answered and the ref is absent - ordinary `default:` / `optional:` territory. It ends a backoff streak rather than extending one, so a ref you provision after the process starts is still picked up on the normal poll interval.
- **It interacts with `WithStale`.** The `*StaleError` handed to `OnError` is escalated on the first failed attempt after `maxAge` elapses, and backoff is what pushes that attempt out - so a large `max` delays that callback by up to one backoff step. `Status()` and `Health()` are unaffected: they recompute `Age` and `Stale` at read time from the last success, so a readiness probe still flips at exactly `maxAge`. Keep `max` well under your `WithStale` threshold if the `OnError` timing matters.

Jitter applies to backoff too. `WithJitter` randomizes each backoff delay by the same fraction it randomizes the poll interval, which matters more here than it does for ordinary polling: a shared backend failing synchronizes every client's failure instant, and un-jittered backoff would have the whole fleet retry in lockstep against a backend that is already unhealthy.

## Tuning the dispatch queue

`OnChange` dispatch goes through a bounded queue. `WithQueueDepth(n)` sets its depth (default 16); when a slow consumer fills it, the oldest queued event is dropped rather than blocking the reconciler. Because each `Change` carries full `Old` / `New` snapshots, a dropped notification never leaves `Get()` wrong.

## See also

- [Source chains](/docs/concepts/source-chains/) for comma-separated precedence and `onfail`.
- [Snapshots and pinning](/docs/usage/snapshots/) for `Status`, history, and pinning.
- [Observability](/docs/observability/) for `Status`, `Health`, and the HTTP surface.
