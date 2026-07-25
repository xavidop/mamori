---
layout: ../../layouts/DocsLayout.astro
title: OpenTelemetry
---

# OpenTelemetry

The core module takes no OpenTelemetry dependency. `WithMeter` and `WithTracer` accept tiny internal interfaces (`mamori.Meter`, `mamori.Tracer`); the `github.com/xavidop/mamori/x/otel` module (package `mamoriotel`) bridges them to real OTel instruments.

```bash
go get github.com/xavidop/mamori/x/otel
```

## Wiring it up

```go
import (
	"go.opentelemetry.io/otel"
	mamoriotel "github.com/xavidop/mamori/x/otel"

	"github.com/xavidop/mamori"
)

func main() {
	meter, err := mamoriotel.NewMeter(otel.Meter("mamori"))
	if err != nil {
		log.Fatal(err)
	}
	tracer := mamoriotel.NewTracer(otel.Tracer("mamori"))

	w, err := mamori.Watch[Config](ctx,
		mamori.WithMeter(meter),
		mamori.WithTracer(tracer),
	)
	// ...
}
```

## What it records

| Instrument | Type | Attributes |
| --- | --- | --- |
| `mamori.resolve.duration` | histogram (ms) | `scheme`, `status` (ok / error), `mamori.error.kind` (failed resolves only) |
| `mamori.refresh.count` | counter | `scheme` |
| `mamori.watch.errors` | counter | `scheme` |

Plus a `mamori.resolve` span per resolve, with attributes `mamori.scheme`, `mamori.ref`, and `mamori.error.kind` (failed resolves only); the span status is set to error (and the error recorded) when a resolve fails.

`mamori.error.kind` is present only when the resolve fails, carrying the same
classification as `mamori.ErrorKind(err)` (see [Concepts](../concepts#error-kinds)):
`not_found`, `permission_denied`, `unauthenticated`, `unavailable`, `rate_limited`,
`invalid`, or `unknown`. It is never set to an empty or placeholder value on
success, so its mere presence on the histogram or the span selects failed
resolves without also checking `status` or the span status.

Because the bridge only implements the small `mamori.Meter` / `mamori.Tracer` interfaces, you can also write your own sink (to Prometheus, statsd, or a test recorder) without pulling in OpenTelemetry at all.
