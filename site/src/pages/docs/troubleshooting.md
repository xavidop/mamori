---
layout: ../../layouts/DocsLayout.astro
title: Troubleshooting
---

# Troubleshooting

Start from what you actually observe, not from a guess at the cause. Each section below names a symptom, the causes worth checking in order, and the specific diagnostic that confirms (or rules out) each one.

## My value is not updating

Check these in order:

1. **The field is polled, not natively watched, and the interval has not elapsed yet.** A polled field only re-resolves every `WithPollInterval` (30s by default), jittered by up to 20%. If you rotated a value 10 seconds ago, waiting is not a bug.
2. **The provider has no native watch at all for this scheme**, so mamori polls it unconditionally regardless of how soon you expect a change. See the Watch column in [Providers](/docs/providers/) for which schemes push and which are polled.
3. **The value genuinely has not changed.** The poller only emits an update when the resolved value actually differs from the last one it saw: it compares the provider's `Version` when either side supplies one, and falls back to a raw byte comparison only when `Version` is empty on both. A backend that reports the same `Version` (or the same bytes) after a "rotation" that did not actually change anything will not produce an event, by design.

Diagnostics:

```go
for _, f := range w.Status().Fields {
	log.Printf("%s: version=%s age=%s stale=%v", f.Path, f.Version, f.Age, f.Stale)
}
```

`w.Status()` shows each field's current `Version`, the age since its last successful resolve, and whether it is stale. If `Age` keeps climbing with no error, the field is not failing, it simply has not changed (or has not been polled again yet).

`WithStale(d)` turns prolonged silence into something visible: once `Age` exceeds `d`, `Status` marks the field stale and `Health` reports it unhealthy, rather than a quiet field looking indistinguishable from a healthy one that just has not rotated lately.

`w.Refresh(ctx)` forces an immediate re-resolve of every field, bypassing the poll interval, and blocks until the result has been applied or rejected. Reach for it when you need to know right now whether a rotation landed, rather than waiting out the next poll tick.

See [Forcing a refresh](/docs/usage/refresh/) for `Refresh` in full, [Observability](/docs/observability/) for `Status` and `Health`, and the [Options reference](/docs/usage/options/) for `WithPollInterval` and the other tuning knobs above.

## My OnChange never fires, or fires once for several changes

Causes:

- **Debounce is coalescing a burst of field changes into one event.** Field changes within the debounce window (500ms by default, overridable per field with a `?debounce=` ref option) are collected and delivered as a single `Change`, not one `Change` per field. Five keys rotating in the same secret is one event, not five.
- **A slow `OnChange` handler let the bounded dispatch queue fill up, and the oldest event was dropped.** `OnChange` delivery goes through a queue (`WithQueueDepth`, 16 by default); if your handler cannot keep up, the queue fills and the oldest pending `Change` is discarded to make room for the newest one.

Diagnostics: `WithDebounce(d)` and `WithQueueDepth(n)` tune the two knobs above; see the [Options reference](/docs/usage/options/) for both. `Meter.RecordChangeDropped()` exists precisely so a dropped event is alertable rather than invisible: wire up a `Meter` and watch that counter.

This is the symptom worth taking seriously, because a dropped change is silent by default. mamori ships with a no-op meter and a logger that discards everything until you configure one, so with no `Meter` and no `WithLogger` installed, a dropped `OnChange` event produces neither a metric nor a log line. Install a `WithLogger`, and the same drop is logged at warning level instead. If your handler occasionally does slow I/O, install a `Meter` before you need it, not after you notice a field looks stuck.

## My update was rejected and Get() still returns the old config

This is the designed behavior, not a bug: mamori refuses an invalid candidate outright and keeps serving the last known-good snapshot rather than applying something broken.

Causes:

- The candidate failed the configured `Validator`.
- A `PreApply` gate rejected it (the credential the candidate carries does not actually work).
- A `WithDerive` hook returned an error (a value assembled from other fields could not be built).

Diagnostics: `OnError` receives a `*ValidationError` for the first case, a `*PreApplyError` for the second, and a `*DeriveError` for the third, so `errors.As` tells them apart:

```go
mamori.OnError(func(err error) {
	var perr *mamori.PreApplyError
	if errors.As(err, &perr) {
		// a PreApply hook refused the candidate
		return
	}
	var verr *mamori.ValidationError
	if errors.As(err, &verr) {
		// the candidate failed struct validation
		return
	}
	var derr *mamori.DeriveError
	if errors.As(err, &derr) {
		// a WithDerive hook returned an error
	}
})
```

`Meter.RecordApplyRejected(reason)` reports which one happened as a `RejectReason`, a closed type with exactly three values, `RejectValidation`, `RejectPreApply`, and `RejectDerive` (a `WithDerive` hook returned an error), so it is safe to use directly as a metric label. See [Rotation safety](/docs/usage/rotation/) for why `PreApply` exists and what a rejection does to `Get()` and `OnChange`.

## My process will not start

`Watch`'s initial load is fail-fast: if a backend it needs is unreachable at startup, `Watch` returns an error immediately rather than starting the watcher with a partial or default-filled configuration. That is deliberate; a service should not come up believing it is configured when it is not.

Diagnostics:

- `mamori.Doctor[T](ctx, opts...)` resolves every field once and reports every failing ref at once, rather than stopping at the first, so you see the whole list of what is unreachable in a single run. Run it as a preflight check before you ever call `Watch` or `Load` for real.
- Look at the error kind. mamori splits resolve failures into terminal kinds (`not_found`, `permission_denied`, `unauthenticated`, `invalid`), which will not clear without a human fixing something, and transient kinds (`unavailable`, `rate_limited`), which are expected to self-heal on a later attempt. A terminal kind at startup usually means a wrong ref or a missing grant; a transient one usually means the backend was not up yet.
- If a field legitimately should not block startup, say so explicitly: tag it `optional:"true"` if its absence is fine, or give it an `onfail:` tag (`onfail:"default"` to fall back to its `default:` value on error, or the reconciler's own `onfail:"keeplast"`/`onfail:"fail"` policies for the watch path) rather than letting an unrelated field's outage take down the whole process.

See [Doctor: pre-deploy check](/docs/observability/doctor/) and [Error kinds](/docs/concepts/error-kinds/) for the full detail on both.

## My secret is showing up in logs

Causes:

- The field is typed as a plain `string` or `[]byte` instead of `secret.String` or `secret.Bytes`, so nothing redacts it when it is logged, formatted, or marshaled.
- Something calls `Reveal()` (the one place plaintext comes out of a secret type) and passes the result straight to a logger, `fmt`, or a response body, bypassing the redaction the wrapper type would otherwise provide.

Diagnostics: `mamori vet ./...` flags the first case, a config field wired to a secret-bearing source but stored in a plain type, and it also runs as a `go vet -vettool` analyzer if you prefer that path. It cannot catch the second case: once you call `Reveal()`, the plaintext is an ordinary Go value and no static check can follow where you send it, so treat every `Reveal()` call site as a spot to review by hand. See [vet](/docs/cli/vet/) for exactly what it flags and [Secret types](/docs/concepts/secret-types/) for what the wrappers redact and why `Reveal()` is deliberately the single, greppable point of access.

## Still stuck?

See [Getting help](/docs/getting-help/) for where to ask, file a bug, or read the API reference. Whatever you file, keep real secret values out of it, redact refs and payloads before pasting them into an issue.
