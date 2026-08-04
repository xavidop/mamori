---
layout: ../../../layouts/DocsLayout.astro
title: Options reference
---

# Options reference

Every `Load`, `Watch`, and `Doctor` call takes the same `...Option` variadic. This page is the single list of all of them, with what each one does and what it defaults to when you leave it unset.

Groups below are ordered by what you are trying to do, not alphabetically, because nobody arrives at this page already knowing an option's name. Where an option has a deeper page, the "What it does" cell links out rather than repeating it here: this page stays a reference, not a second copy of [Watching](/docs/usage/watching/), [Rotation safety](/docs/usage/rotation/), [Observability](/docs/observability/), or [Telemetry](/docs/telemetry/).

## Sources and decoding

| Option | What it does | Default |
| --- | --- | --- |
| `WithProvider` | Registers a provider for this call only, taking precedence over the global registry for its scheme. | none (falls back to the global registry) |
| `WithRefVars` | Supplies the variables available to `${VAR}` expansion in `source` tags. | none (no variables; a tag using `${VAR}` errors without this) |
| `WithDecodeHook` | Adds a mapstructure decode hook applied when decoding a `flatten:"json\|yaml\|toml\|env"` payload; see [Value decoding](/docs/concepts/decoding/). | none (only the built-in secret/duration hook runs) |
| `WithValidator` | Overrides the validator used on load and on every reconciled update; see [Validation](/docs/validation/). | the built-in go-playground/validator-based validator |
| `WithExecProvider` | Enables the opt-in `exec:` provider for this call; see [exec](/docs/providers/exec/). | disabled (not registered) |

## Cadence and retry

| Option | What it does | Default |
| --- | --- | --- |
| `WithPollInterval` | Sets the fallback poll interval for providers with no native watch. | 30s |
| `WithJitter` | Randomizes each poll interval by this fraction, to avoid a thundering herd. | 0.2 (plus or minus 20%) |
| `WithBackoff` | Enables per-ref exponential backoff on resolve failure, in place of the plain poll interval; see [Retry backoff](/docs/usage/watching/#retry-backoff). | disabled, see the callout below |
| `WithStale` | Escalates prolonged staleness to a `*StaleError` delivered to `OnError`. | disabled (0) |
| `WithClock` | Overrides the clock, primarily for deterministic tests. | the real system clock |

## Surviving a cold start during an outage

| Option | What it does | Default |
| --- | --- | --- |
| `WithBootstrapCache` | Keeps an encrypted, on-disk snapshot of the last known-good resolved values and boots from it when a cold start cannot reach the backend; see [Bootstrap cache](/docs/usage/bootstrap-cache/). | disabled (no snapshot is ever written) |
| `BootstrapMaxAge` | A `BootstrapOption` passed to `WithBootstrapCache`: how old a restored snapshot may be while `Health` still reports the process ready. `0` means unbounded; a negative value is refused with `ErrInvalid`. | 24h (`DefaultBootstrapMaxAge`) |

### The snapshot is a credential at rest

**Enabling `WithBootstrapCache` creates a file holding live credentials that did not exist before.** It is sealed with AES-256-GCM and written `0600`. See [Bootstrap cache](/docs/usage/bootstrap-cache/) for how to supply the key, which failures fall back, and what to set `BootstrapMaxAge` to.

### Backoff is disabled by default

**Without an explicit `WithBackoff(base, max)`, there is no retry backoff at all.** A ref that fails to resolve is simply retried on the plain `WithPollInterval` cadence, forever, until it succeeds or `WithStale` escalates the staleness to a hard error. It is easy to assume some backoff already exists once you start tuning retry behavior; it does not, and that is deliberate: turning it on by default would silently have changed the retry cadence of every caller already running mamori. See [Retry backoff](/docs/usage/watching/#retry-backoff) for the full behavior, including how it interacts with `WithStale` and `WithJitter`, once you do enable it.

## Reacting to change

| Option | What it does | Default |
| --- | --- | --- |
| `OnChange` | Installs the callback invoked once per applied update; see [React to a field with Change and Changed](/docs/usage/watching/#react-to-a-field-with-change-and-changed). | none |
| `OnError` | Installs a callback for runtime resolve, validation, stale, PreApply-rejection, and derive-rejection errors; see [Handle errors with OnError](/docs/usage/watching/#handle-errors-with-onerror). | none |
| `PreApply` | Installs a gate that must pass before a candidate snapshot becomes current; see [Rotation safety](/docs/usage/rotation/). | none (no gate) |
| `WithPreApplyTimeout` | Bounds how long a `PreApply` hook may run. | 10s |
| `WithDerive` | Computes fields from already-resolved fields, after decoding and before validation, and declares which fields it writes so they appear in `ev.Changed()` and `Status()`; see [Derived fields](/docs/usage/derived-fields/). | none (no derivation runs) |
| `WithDebounce` | Sets the coalescing window for change events. | 500ms |
| `WithQueueDepth` | Bounds the `OnChange` dispatch queue; the oldest event is dropped once it fills. | 16 |
| `WithHistory` | Retains the `n` most recent snapshots beyond the current one, for [pinning](/docs/usage/snapshots/). | 0 (current snapshot only) |

## Observability

| Option | What it does | Default |
| --- | --- | --- |
| `WithLogger` | Installs a structured `slog.Logger` for engine events; see [Logging](/docs/telemetry/logging/). | discards everything (mamori logs nothing by default) |
| `WithMeter` | Installs a metrics sink; see [Telemetry](/docs/telemetry/). | a no-op meter |
| `WithTracer` | Installs a tracing sink; see [OpenTelemetry](/docs/telemetry/opentelemetry/). | a no-op tracer |

## Admin endpoint

| Option | What it does | Default |
| --- | --- | --- |
| `WithAdminHTTP` | Makes `Watch` run its own HTTP server exposing the [admin endpoint](/docs/observability/admin/); `Load` accepts it but ignores it, having no watcher to serve. | off (no listener bound, no port taken) |
| `WithAdminTLS` | Serves the `WithAdminHTTP` endpoint over TLS, with optional mutual TLS. | off (plaintext; has no effect without `WithAdminHTTP`) |

## Handler options are not Options

`WithAuth`, `HandlerPrefix`, and `HandlerMiddleware` return `HandlerOption`, a distinct type from `Option`. They configure the admin HTTP handler built by `mamori.Handler` or passed through `WithAdminHTTP`'s variadic `opts`, and are never passed directly to `Load` or `Watch`. Mixing the two types is a compile error, and that is deliberate: it keeps "configures the watcher" and "configures the HTTP handler" from being interchangeable at the call site.

| Option | What it does | Default |
| --- | --- | --- |
| `WithAuth` | Requires every request to pass an `Authenticator` before it is served (`/healthz` stays exempt from credentials, though not from the failing-field detail); see [Auth](/docs/auth/). | none (no auth; every request is served) |
| `HandlerPrefix` | Strips a path prefix before routing, so the handler can be mounted under a subpath of an existing mux. | none (mounted at the mux root) |
| `HandlerMiddleware` | Wraps the handler with a non-auth concern such as request logging or rate limiting, applied outside `WithAuth`. | none |

## Which options reach WatchRef

Only the clock, poll interval, jitter, and backoff window reach `WatchRef`. See [Watching one ref](/docs/usage/watch-ref/#which-options-apply) for which options those are and why the rest, including `WithValidator` and `PreApply`, have no effect there.

## See also

- [Watching](/docs/usage/watching/) for `Watch`, `OnChange`, and `OnError` in context.
- [Rotation safety](/docs/usage/rotation/) for `PreApply` end to end.
- [Snapshots and pinning](/docs/usage/snapshots/) for `WithHistory`, `Status`, and pinning.
- [Observability](/docs/observability/) and [Telemetry](/docs/telemetry/) for `WithLogger`, `WithMeter`, and `WithTracer`.
