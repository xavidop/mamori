# mamori Prometheus bridge (`x/prom`)

`github.com/xavidop/mamori/x/prom` implements `mamori.Meter` directly against
[`prometheus/client_golang`](https://github.com/prometheus/client_golang), for
shops that run Prometheus without adopting OpenTelemetry. This keeps the core
`mamori` module free of any Prometheus dependency: you only pull in
`client_golang` if you opt into this bridge.

If you already use OpenTelemetry, prefer
[`x/otel`](../otel/) with a Prometheus exporter instead of this module: OTel's
Prometheus exporter gives you the same `/metrics` endpoint plus spans, and one
metrics pipeline rather than two. Reach for `x/prom` when `client_golang` is
the only metrics dependency you want in your build.

The Go package is named **`prom`**.

## Install

```sh
go get github.com/xavidop/mamori/x/prom
```

## What it does

`New` wraps a `prometheus.Registerer` and registers six instruments, recording
to them as mamori resolves and reconciles config. It is safe for concurrent
use.

## Usage

Build a `prometheus.Registry` (or use `prometheus.DefaultRegisterer`), wrap it
with `New`, and pass the result to `mamori.WithMeter`:

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

	cfg, err := mamori.Load[Config](ctx,
		mamori.WithMeter(meter),
	)
	if err != nil {
		return err
	}
	_ = cfg

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return http.ListenAndServe(":9090", nil)
}
```

`New` returns an error if any instrument fails to register, matching
`x/otel`'s `NewMeter`: register twice against the same `Registerer`, or
collide with an existing collector, and you get an error back instead of a
panic.

## Metrics

| Instrument | Name | Kind | Unit | Labels |
| --- | --- | --- | --- | --- |
| Resolve duration | `mamori_resolve_duration_seconds` | Histogram | seconds | `scheme`, `status` (`ok` \| `error`), `error_kind` |
| Refresh count | `mamori_refresh_total` | Counter | - | `scheme` |
| Watch errors | `mamori_watch_errors_total` | Counter | - | `scheme` |
| Stale count | `mamori_stale_total` | Counter | - | `scheme` |
| Change dropped count | `mamori_change_dropped_total` | Counter | - | none |
| Apply rejected count | `mamori_apply_rejected_total` | Counter | - | `reason` (`validation` \| `preapply`) |

- `scheme` is the provider scheme of the resolved ref (e.g. `file`, `aws-sm`,
  `vault`).
- `status` is `error` when the resolve returned a non-nil error, otherwise
  `ok`.
- `error_kind` carries the same classification as `mamori.ErrorKind(err)`:
  `not_found`, `permission_denied`, `unauthenticated`, `unavailable`,
  `rate_limited`, `invalid`, or `unknown`. Unlike `x/otel`, which omits the
  attribute entirely on success, `error_kind` here is the empty string on a
  successful resolve rather than absent: a Prometheus `HistogramVec` requires
  every series to share the same label set, so there is no "attribute not
  present" to fall back on. Filter on `status="error"` (or `error_kind!=""`)
  to select failures.
- `mamori_change_dropped_total` carries no labels at all: the dispatch queue
  it reports on is a process-wide property, not a per-scheme one. A non-zero
  rate means an `OnChange` handler is not keeping up with the rate of applied
  changes, and callers are missing changes as a result.
- `reason` on the apply-rejected counter is `mamori.RejectReason`, a closed set
  of two values (`validation`, `preapply`) so it stays a safe, bounded metric
  label.

**Seconds, not milliseconds.** `mamori_resolve_duration_seconds` records in
seconds, following Prometheus convention for a duration histogram
(the `_seconds` suffix is the convention, not just a naming choice).
`x/otel`'s `mamori.resolve.duration` records the same event in
**milliseconds**. If you run both bridges (or compare a Prometheus dashboard
against an OTel one for the same event), the numbers will differ by a factor
of 1000; that difference is expected, not a bug in either bridge.

The instrument names are also exported as constants (`MetricResolveDuration`,
`MetricRefreshTotal`, `MetricWatchErrorsTotal`, `MetricStaleTotal`,
`MetricChangeDroppedTotal`, `MetricApplyRejectedTotal`).

## The cardinality and credential-leak guard

Every label above is drawn from a small, closed set: a provider scheme, a
fixed `ok`/`error` status, a `mamori.Kind`, or a `mamori.RejectReason`. None of
them is a ref or a resolved value.

This matters for two reasons at once:

- **Cardinality.** A ref is effectively unbounded (it can carry a path, a
  version, a query string). A label with unbounded cardinality is a
  well-documented way to take down a Prometheus server or the TSDB behind it.
- **Credential leakage.** A ref can carry an inline credential, such as
  `?token=...`. A label is stored, scraped, retained, and often forwarded to a
  third-party observability backend; a value there is far more exposed than
  the same string in a log line that redaction already covers.

`x/prom` never accepts a ref or a resolved value as an argument in the first
place: `mamori.Meter`'s methods take only a `scheme`, a `time.Duration`, an
`error`, or a `mamori.RejectReason`, so there is no plumbing through which
either could reach a label.

## Development

This module lives two levels below the repo root and uses a local `replace`
directive, so run every `go` command with the workspace disabled:

```sh
cd x/prom
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

Tests use `prometheus/client_golang/prometheus/testutil` against an in-memory
`prometheus.Registry` - no scrape endpoint or Prometheus server is required.
