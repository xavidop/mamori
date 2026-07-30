---
layout: ../../layouts/DocsLayout.astro
title: OpenTelemetry
---

# OpenTelemetry

The `github.com/xavidop/mamori/x/otel` bridge (package `mamoriotel`) turns mamori's config resolves into OpenTelemetry spans and metrics: one `mamori.resolve` span per resolve, plus latency, refresh, watch-error, stale, dropped-change, and rejected-apply instruments, all tagged with the provider scheme and, on failure, a `mamori.error.kind` classification. Reach for it when you already run OTel and want config resolution to show up in the same traces and dashboards as the rest of your service.

## Quick start

Install the bridge module (the core `mamori` module stays free of any OpenTelemetry dependency):

```bash
go get github.com/xavidop/mamori/x/otel
```

Wrap an OTel meter and tracer with `NewMeter` / `NewTracer`, then pass the results to `mamori.WithMeter` / `mamori.WithTracer`:

```go
package main

import (
	"context"
	"log"

	"go.opentelemetry.io/otel"

	"github.com/xavidop/mamori"
	mamoriotel "github.com/xavidop/mamori/x/otel"
)

type Config struct {
	Port int `mamori:"file:///etc/app.yaml#port"`
}

func run(ctx context.Context) error {
	// otel.Meter / otel.Tracer come from your configured OTel providers;
	// substitute your own MeterProvider / TracerProvider if you do not use
	// the globals.
	meter, err := mamoriotel.NewMeter(otel.Meter("mamori"))
	if err != nil {
		return err
	}
	tracer := mamoriotel.NewTracer(otel.Tracer("mamori"))

	w, err := mamori.Watch[Config](ctx,
		mamori.WithMeter(meter),
		mamori.WithTracer(tracer),
	)
	if err != nil {
		return err
	}
	defer w.Close()

	log.Printf("port: %d", w.Get().Port)
	return nil
}
```

Every resolve now emits a `mamori.resolve` span and records to `mamori.resolve.duration`; failed resolves also carry `mamori.error.kind`.

## Trace each resolve

`NewTracer` wraps an OTel `trace.Tracer` and starts one span per resolve, ending it with the correct status (and a recorded error on failure). Pass the result to `mamori.WithTracer`:

```go
tracer := mamoriotel.NewTracer(otel.Tracer("mamori"))

w, err := mamori.Watch[Config](ctx, mamori.WithTracer(tracer))
```

Each resolve produces one span, summarized below:

| Field | Value |
| --- | --- |
| Name | `mamori.resolve` (exported as `SpanResolve`) |
| Attributes | `mamori.scheme`, `mamori.ref`, and `mamori.error.kind` (failed resolves only) |
| Status on success | `Ok` |
| Status on failure | `Error`, with the error message as the description and the error recorded as an `exception` span event via `RecordError` |

## Record metrics

`NewMeter` wraps an OTel `metric.Meter` and registers six instruments up front, recording to them as mamori resolves and reconciles config. Pass the result to `mamori.WithMeter`:

```go
meter, err := mamoriotel.NewMeter(otel.Meter("mamori"))
if err != nil {
	return err
}

w, err := mamori.Watch[Config](ctx, mamori.WithMeter(meter))
```

The six instruments and their attributes:

| Instrument | Name | Kind | Unit | Attributes |
| --- | --- | --- | --- | --- |
| Resolve duration | `mamori.resolve.duration` | Float64 histogram | `ms` | `scheme`, `status` (`ok` \| `error`), `mamori.error.kind` (failed resolves only) |
| Refresh count | `mamori.refresh.count` | Int64 counter | - | `scheme` |
| Watch errors | `mamori.watch.errors` | Int64 counter | - | `scheme` |
| Stale count | `mamori.stale.count` | Int64 counter | - | `scheme` |
| Change dropped count | `mamori.change.dropped.count` | Int64 counter | - | none |
| Apply rejected count | `mamori.apply.rejected.count` | Int64 counter | - | `reason` (`validation` \| `preapply`) |

- `scheme` is the provider scheme of the resolved ref (e.g. `file`, `aws`, `vault`).
- `status` is `error` when the resolve returned a non-nil error, otherwise `ok`.
- `mamori.change.dropped.count` carries no attributes at all: the bounded `OnChange` dispatch queue it reports on is a process-wide property, not a per-scheme one. **This is the counter to alert on**: a non-zero rate means an `OnChange` handler is not keeping up with the rate of applied changes, and the oldest change events are being silently discarded as a result.
- `reason` on the apply-rejected counter carries `mamori.RejectReason`, a closed set of exactly two values (`validation`, `preapply`) so it stays a safe, bounded metric label rather than an unbounded free-form string.

The instrument names are also exported as constants (`MetricResolveDuration`, `MetricRefreshCount`, `MetricWatchErrors`, `MetricStaleCount`, `MetricChangeDroppedCount`, `MetricApplyRejectedCount`).

## Log engine events

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

## The `mamori.error.kind` attribute

`mamori.error.kind` is the useful differentiator for dashboards: it lets you separate a denied permission from a rate limit from a missing key rather than lumping every failure into one `error` bucket. It appears on both the `mamori.resolve.duration` histogram and the `mamori.resolve` span.

It is present only when the resolve fails, carrying the same classification as `mamori.ErrorKind(err)` (see [Concepts](/docs/concepts/error-kinds/)):

- `not_found`
- `permission_denied`
- `unauthenticated`
- `unavailable`
- `rate_limited`
- `invalid`
- `unknown`

It is never set to an empty or placeholder value on success, so its mere presence on the histogram or the span selects failed resolves without also checking `status` or the span status.

## How it works

The core module takes no OpenTelemetry dependency. `WithMeter` and `WithTracer` accept tiny internal interfaces (`mamori.Meter`, `mamori.Tracer`), and this bridge is what adapts a real OTel `metric.Meter` and `trace.Tracer` to them. You only pull in OTel if you opt into the bridge.

The Go package is named `mamoriotel` (rather than `otel`) so it can be imported alongside `go.opentelemetry.io/otel` without a name clash. `NewMeter` returns an error if any instrument fails to register, and the meter records measurements against `context.Background()`. Both adapters are safe for concurrent use.

Because the bridge only implements the small `mamori.Meter` / `mamori.Tracer` interfaces, you can also write your own sink (to Prometheus, statsd, or a test recorder) without pulling in OpenTelemetry at all. `mamori.Meter` has six methods (`RecordResolve`, `RecordRefresh`, `RecordWatchError`, `RecordStale`, `RecordChangeDropped`, `RecordApplyRejected`); a hand-written implementation must provide all six. `RecordApplyRejected` takes a `mamori.RejectReason`, a closed string type with exactly two values (`mamori.RejectValidation`, `mamori.RejectPreApply`) so it is safe to use as a metric label without risking unbounded cardinality.

## See also

[Observability](/docs/observability/) covers `Status`, `Health`, and the pre-deploy `Doctor` check, which answer "what is true right now" where the spans and metrics here answer "what happened over time."

[Concepts](/docs/concepts/error-kinds/) covers the error-kind classification that `mamori.error.kind` carries.
