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

A [`WithDerive`](/docs/usage/derived-fields/)-declared field appears in `Fields` like any other when its rebuilt value changes. Its versions are content hashes of the value rather than provider revisions, since it has no ref.

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("DBPassword") {
		pool.Rotate(ev.New.DBPassword.Reveal())
	}
})
```

## Handle errors with OnError

`OnError` receives every runtime failure without stopping the watcher. Use it for metrics and alerting; `Get()` keeps serving the last good config.

Five typed errors reach it, and they mean different things, so classify rather than counting them together: `*ProviderError` (a ref failed to resolve), `*ValidationError` (the candidate failed `validate:` tags), `*DeriveError` (a [`WithDerive`](/docs/usage/derived-fields/) hook returned an error), `*PreApplyError` (a [`PreApply`](/docs/usage/rotation/) gate refused the candidate, including on timeout), and `*StaleError` (a ref went unrefreshed past `WithStale`).

```go
mamori.OnError(func(err error) {
	var verr *mamori.ValidationError
	var derr *mamori.DeriveError
	switch {
	case errors.As(err, &verr):
		metrics.Inc("config_validation_error")
	case errors.As(err, &derr):
		metrics.Inc("config_derive_error")
	default:
		metrics.Inc("config_error")
	}
})
```

The middle three all mean the same thing operationally: a candidate was built and rejected, so the config you are serving is older than the backend's. `*ProviderError` means a resolve failed. `*StaleError` means a ref has gone too long without a fresh value, not that it never had one: the last good value is still being served, which is exactly why the age is worth alerting on.

## What you can rely on

These behaviors are guaranteed and covered by the conformance kit.

- **Validated, all-or-nothing updates.** `OnChange` fires with a fully re-validated snapshot. If a new value fails validation the update is rejected: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`.
- **A gate before the swap, if you install one.** A candidate is built, then any `WithDerive` hooks run, then it is validated, then - if `PreApply` was installed - handed to it for a check that needs I/O (a rotated password actually opens a connection). Only after that gate passes does the atomic swap happen and `OnChange` fire. See [Rotation safety](/docs/usage/rotation/) and [Derived fields](/docs/usage/derived-fields/).
- **OnChange is called one at a time.** Callbacks are serialized, so your callback never runs concurrently with itself. The dispatch queue absorbs bursts, so a slow callback just delays the next event; only once a handler stays slower than the change rate for long enough to fill the queue does it overflow, and the oldest queued event is dropped. See [Tuning the dispatch queue](#tuning-the-dispatch-queue) below.
- **Coalesced events.** Field changes within a debounce window (default 500ms, override per field with `?debounce=`) produce a single `Change`. A JSON secret with five keys rotating is one event, not five. See the [Options reference](/docs/usage/options/) for this and every other tuning knob's default.
- **Last-good on failure.** On a runtime resolve failure the last-good value is retained, `OnError` receives a `*ProviderError`, and the ref keeps being retried - on the poll interval by default, or with per-ref exponential backoff if you opt into it with [`WithBackoff`](#retry-backoff). `WithStale(maxAge)` escalates prolonged staleness to a hard `*StaleError`.
- **Clean shutdown.** `Close()` cancels provider watches, drains the callback queue, and returns.
- **`OnChange` is type-checked against `Watch`'s own `T`.** `OnChange[T]` has to match the `T` passed to `Watch[T]`/`Load[T]`; a mismatch (a type alias, a renamed config struct, a generic helper passing the wrong type through) fails `Watch`/`Load` outright with an error wrapping `ErrInvalid` that names both types, rather than compiling clean and then never calling the callback.

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
