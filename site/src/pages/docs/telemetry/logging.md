---
layout: ../../../layouts/DocsLayout.astro
title: Logging
---

# Logging

Structured logs of what the engine did and when, through `log/slog`.

Unlike `WithMeter` and `WithTracer`, logging needs no bridge module: `WithLogger` takes a standard library `*slog.Logger` directly, so it works with the core `mamori` module alone.

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithLogger(slog.Default()),
)
```

mamori logs nothing until you pass this option. The zero configuration is a discard logger, so linking mamori into an application never writes a line to that application's stderr on its own - a library that did would be making a decision that belongs to the application.

Every event mamori can log:

| Message | Level | When |
| --- | --- | --- |
| `resolve failed` | Warn | A one-shot `Load`, or a `Watch`'s initial resolve, could not reach the provider or the provider returned an error. |
| `watch error` | Warn | A runtime watch or poll delivered a non-not-found error for a field. |
| `value is stale` | Warn | In addition to `watch error`, once the field has gone unrefreshed longer than the `WithStale` threshold. |
| `resolve recovered` | Info | A field that previously carried an error resolved cleanly (or fell back to a tolerated absence); logged only when there was an error to recover from, so a healthy refresh stays quiet. |
| `candidate rejected by validation; continuing to serve the previous config` | Error | A reconciled candidate failed struct validation and was discarded. |
| `change rejected by PreApply; continuing to serve the previous config` | Warn | A `PreApply` hook rejected a candidate. |
| `config change applied` | Info | A reconciled snapshot was accepted; logged once per flush, with the number of changed fields. |
| `field updated` | Debug | One record per field included in an applied change, so the detail is available without making the Info line unbounded. |
| `change event dropped, dispatch queue full; the OnChange handler is not keeping up` | Warn | The bounded `OnChange` dispatch queue was full and the oldest event was dropped. |
| `provider has no native watch, polling` | Debug | A ref's provider does not implement `WatchableProvider`, so mamori falls back to polling it. |

Two guarantees hold across every one of these records.

A resolved value never appears. Records carry the field path, the provider scheme, the ref (with sensitive query options like `?token=` redacted the same way `Status` redacts them), the version, and the error - never `Value.Bytes` or a decoded field. A configuration log is exactly the artifact most likely to be shipped off the host to a collector, so this is tested, not just documented.

The handler runs on the reconciler goroutine, the same constraint `OnError` carries: a handler that blocks (writing synchronously to a remote collector, for instance) stalls reconciliation for as long as it runs. Buffer it if it needs to do I/O.

`WithLogger` and `OnError` are independent and can both be set; an error reaches both, and neither suppresses the other.

## See also

- [OpenTelemetry](/docs/telemetry/opentelemetry/) and [Prometheus](/docs/telemetry/prometheus/) count the same events. A log tells you what happened to one field; a counter tells you how often it happens across all of them, which is what you alert on.
- [Observability](/docs/observability/) answers what is true right now, rather than what happened over time.
