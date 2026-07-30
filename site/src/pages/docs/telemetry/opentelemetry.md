---
layout: ../../../layouts/DocsLayout.astro
title: OpenTelemetry
---

# OpenTelemetry

The `github.com/xavidop/mamori/x/otel` bridge (package `mamoriotel`) turns mamori's config resolves into OpenTelemetry spans and metrics: one `mamori.resolve` span per resolve, plus latency, refresh, watch-error, stale, dropped-change, and rejected-apply instruments, all tagged with the provider scheme and, on failure, a `mamori.error.kind` classification. Reach for it when you already run OTel and want config resolution to show up in the same traces and dashboards as the rest of your service.

Running Prometheus without OpenTelemetry? See [Prometheus](/docs/telemetry/prometheus/) for `x/prom`, a sibling bridge that implements `mamori.Meter` directly against `prometheus/client_golang` instead of going through OTel.

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

Logging needs no bridge module: `WithLogger` takes a standard library `*slog.Logger` directly, so it works with the core `mamori` module alone and has nothing to do with OpenTelemetry.

See [Logging](/docs/telemetry/logging/).

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

[Prometheus](/docs/telemetry/prometheus/) covers the sibling bridge, `x/prom`: the same `mamori.Meter` surface implemented directly against `prometheus/client_golang`, for shops that have not adopted OpenTelemetry.

[Observability](/docs/observability/) covers `Status`, `Health`, and the pre-deploy `Doctor` check, which answer "what is true right now" where the spans and metrics here answer "what happened over time."

[Concepts](/docs/concepts/error-kinds/) covers the error-kind classification that `mamori.error.kind` carries.
