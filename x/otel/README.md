# mamori OpenTelemetry bridge (`x/otel`)

`github.com/xavidop/mamori/x/otel` adapts an OpenTelemetry `metric.Meter` and
`trace.Tracer` to mamori's minimal `mamori.Meter` and `mamori.Tracer`
interfaces. This keeps the core `mamori` module free of any OpenTelemetry
dependency: you only pull in OTel if you opt into this bridge.

The Go package is named **`mamoriotel`** (not `otel`) so it can be imported
alongside `go.opentelemetry.io/otel` without a name clash.

## Install

```sh
go get github.com/xavidop/mamori/x/otel
```

## What it does

- **Metrics** - `NewMeter` wraps an OTel `metric.Meter` and registers three
  instruments, recording to them as mamori resolves and reconciles config.
- **Tracing** - `NewTracer` wraps an OTel `trace.Tracer` and starts one span per
  resolve, ending it with the correct status (and a recorded error on failure).

Both adapters are safe for concurrent use.

## Usage

Create your OTel meter and tracer from whatever provider you configure
(exporters, resource, etc.), wrap them with this bridge, and pass the results to
`mamori.WithMeter` / `mamori.WithTracer`:

```go
package main

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/xavidop/mamori"
	mamoriotel "github.com/xavidop/mamori/x/otel"
)

type Config struct {
	Port int `mamori:"file:///etc/app.yaml#port"`
}

func run(ctx context.Context) error {
	// otel.Meter / otel.Tracer come from the globally-configured OTel
	// providers; substitute your own MeterProvider / TracerProvider if you
	// do not use the globals.
	meter, err := mamoriotel.NewMeter(otel.Meter("mamori"))
	if err != nil {
		return err
	}
	tracer := mamoriotel.NewTracer(otel.Tracer("mamori"))

	cfg, err := mamori.Load[Config](ctx,
		mamori.WithMeter(meter),
		mamori.WithTracer(tracer),
	)
	if err != nil {
		return err
	}
	_ = cfg
	return nil
}
```

`NewMeter` returns an error if any instrument fails to register. The meter
records measurements against `context.Background()`.

## Metrics

| Instrument | Name | Kind | Unit | Attributes |
| --- | --- | --- | --- | --- |
| Resolve duration | `mamori.resolve.duration` | Float64 histogram | `ms` | `scheme`, `status` (`ok` \| `error`) |
| Refresh count | `mamori.refresh.count` | Int64 counter | - | `scheme` |
| Watch errors | `mamori.watch.errors` | Int64 counter | - | `scheme` |

- `scheme` is the provider scheme of the resolved ref (e.g. `file`, `aws`,
  `vault`).
- `status` is `error` when the resolve returned a non-nil error, otherwise `ok`.

The instrument names and metric attribute keys are also exported as constants
(`MetricResolveDuration`, `MetricRefreshCount`, `MetricWatchErrors`).

## Traces

Each resolve produces one span:

- **Name**: `mamori.resolve` (exported as `SpanResolve`)
- **Attributes**: `mamori.scheme`, `mamori.ref`
- **Status on success**: `Ok`
- **Status on failure**: `Error` with the error message as the description, plus
  the error recorded as an `exception` span event via `RecordError`.

## Development

This module lives two levels below the repo root and uses a local `replace`
directive, so run every `go` command with the workspace disabled:

```sh
cd x/otel
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

Tests use the in-memory OTel SDK (`sdk/metric` manual reader + `metricdata`, and
`sdk/trace` with a `tracetest.SpanRecorder`) - no exporter or collector is
required.
