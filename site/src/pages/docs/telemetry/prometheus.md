---
layout: ../../../layouts/DocsLayout.astro
title: Prometheus
---

# Prometheus

The `github.com/xavidop/mamori/x/prom` bridge (package `prom`) implements `mamori.Meter` directly against `prometheus/client_golang`, registering six instruments up front: resolve latency, refresh, watch-error, stale, dropped-change, and rejected-apply, all labeled with the provider scheme and, on failure, an `error_kind` classification. Reach for it when you run Prometheus without OpenTelemetry and want `client_golang` to be the only metrics dependency in your build.

Already on OpenTelemetry? Use [`x/otel`](/docs/telemetry/opentelemetry/) with a Prometheus exporter instead of this module: it gives you the same `/metrics` endpoint plus spans, through one metrics pipeline rather than two. `x/prom` exists specifically for shops using `client_golang` directly.

## Quick start

Install the bridge module (the core `mamori` module stays free of any Prometheus dependency):

```bash
go get github.com/xavidop/mamori/x/prom
```

Wrap a `prometheus.Registerer` with `New`, pass the result to `mamori.WithMeter`, and serve the registry over HTTP with `promhttp`:

```go
package main

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xavidop/mamori"
	mamoriprom "github.com/xavidop/mamori/x/prom"
)

type Config struct {
	Port int `mamori:"file:///etc/app.yaml#port"`
}

func run(ctx context.Context) error {
	reg := prometheus.NewRegistry()

	meter, err := mamoriprom.New(reg)
	if err != nil {
		return err
	}

	w, err := mamori.Watch[Config](ctx, mamori.WithMeter(meter))
	if err != nil {
		return err
	}
	defer w.Close()

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return http.ListenAndServe(":9090", nil)
}
```

Every resolve now records to `mamori_resolve_duration_seconds`; failed resolves also carry `error_kind`.

## Record metrics

`New` registers six instruments up front, recording to them as mamori resolves and reconciles config. Pass the result to `mamori.WithMeter`:

```go
meter, err := mamoriprom.New(reg)
if err != nil {
	return err
}

w, err := mamori.Watch[Config](ctx, mamori.WithMeter(meter))
```

The six instruments and their labels:

| Instrument | Name | Kind | Unit | Labels |
| --- | --- | --- | --- | --- |
| Resolve duration | `mamori_resolve_duration_seconds` | Histogram | seconds | `scheme`, `status` (`ok` \| `error`), `error_kind` |
| Refresh count | `mamori_refresh_total` | Counter | - | `scheme` |
| Watch errors | `mamori_watch_errors_total` | Counter | - | `scheme` |
| Stale count | `mamori_stale_total` | Counter | - | `scheme` |
| Change dropped count | `mamori_change_dropped_total` | Counter | - | none |
| Apply rejected count | `mamori_apply_rejected_total` | Counter | - | `reason` (`validation` \| `preapply`) |

- `scheme` is the provider scheme of the resolved ref (e.g. `file`, `aws-sm`, `vault`).
- `status` is `error` when the resolve returned a non-nil error, otherwise `ok`.
- `error_kind` carries the same classification as `mamori.ErrorKind(err)`. Unlike `x/otel`, which omits its equivalent attribute entirely on success, `error_kind` here is the **empty string** on a successful resolve rather than absent - a Prometheus `HistogramVec` requires every series to share the same label set, so there is no "attribute not present" to fall back on. Filter on `status="error"` to select failures.
- `mamori_change_dropped_total` carries no labels at all: the bounded `OnChange` dispatch queue it reports on is a process-wide property, not a per-scheme one. **This is the counter to alert on**: a non-zero rate means an `OnChange` handler is not keeping up with the rate of applied changes, and the oldest change events are being silently discarded as a result.
- `reason` on the apply-rejected counter carries `mamori.RejectReason`, a closed set of exactly two values (`validation`, `preapply`) so it stays a safe, bounded metric label rather than an unbounded free-form string.

The instrument names are also exported as constants (`MetricResolveDuration`, `MetricRefreshTotal`, `MetricWatchErrorsTotal`, `MetricStaleTotal`, `MetricChangeDroppedTotal`, `MetricApplyRejectedTotal`).

### Seconds, not milliseconds

`mamori_resolve_duration_seconds` records resolve duration in **seconds**, following the Prometheus convention that a duration histogram's name carries the unit it measures in. [`x/otel`'s `mamori.resolve.duration`](/docs/telemetry/opentelemetry/#record-metrics) records the very same event in **milliseconds**, per OpenTelemetry's own convention. If you run both bridges side by side, or compare a Prometheus dashboard against an OTel-backed one for the same resolve, the numbers differ by a factor of 1000 - that is expected, not a bug in either bridge.

## The cardinality and credential-leak guard

Every label above is drawn from a small, closed set: a provider scheme, a fixed `ok`/`error` status, a `mamori.Kind`, or a `mamori.RejectReason`. None of them is a ref or a resolved value, and `x/prom` has no way to accept one: `mamori.Meter`'s methods take only a scheme, a duration, an error, or a `mamori.RejectReason`, so there is no parameter through which a ref or a value could reach a label.

This is deliberate, not incidental, and it matters for two separate reasons:

- **Cardinality.** A ref is effectively unbounded (it can carry a path, a version, a query string). An unbounded label value is a well-known way to overwhelm a Prometheus server or the time-series database behind it.
- **Credential leakage.** A ref can carry an inline credential, such as `?token=...`. A label is stored, scraped, retained, and often forwarded to a third-party observability backend, which exposes it far more broadly than the same string in a log line - and mamori's log records already redact exactly this kind of ref (see [OpenTelemetry](/docs/telemetry/opentelemetry/) for the logging contract).

## How it works

The core module takes no Prometheus dependency. `WithMeter` accepts the tiny internal `mamori.Meter` interface, and this bridge is what adapts a real `prometheus.Registerer` to it. You only pull in `client_golang` if you opt into the bridge.

`New` returns an error if any instrument fails to register (a duplicate registration against the same `Registerer`, for instance), rather than panicking, matching `x/otel`'s `NewMeter`. The meter is safe for concurrent use.

Because both bridges only implement the small `mamori.Meter` interface, you can also write your own sink (to statsd, a test recorder, or anything else) without pulling in either dependency. `mamori.Meter` has six methods (`RecordResolve`, `RecordRefresh`, `RecordWatchError`, `RecordStale`, `RecordChangeDropped`, `RecordApplyRejected`); a hand-written implementation must provide all six.

## See also

[OpenTelemetry](/docs/telemetry/opentelemetry/) covers the sibling bridge: same `mamori.Meter` surface, plus tracing, for shops already on OTel. Use it with a Prometheus exporter if you want both a `/metrics` endpoint and spans through one pipeline.

[Observability](/docs/observability/) covers `Status`, `Health`, and the pre-deploy `Doctor` check, which answer "what is true right now" where the metrics here answer "what happened over time."

[Concepts](/docs/concepts/error-kinds/) covers the error-kind classification that `error_kind` carries.
