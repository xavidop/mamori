package mamori

import (
	"context"
	"time"
)

// Meter is the minimal metrics sink mamori emits to. It is deliberately tiny so
// the core module takes no OpenTelemetry dependency; the x/otel module provides
// an adapter that implements this interface on top of an OTel meter. Pass one
// with WithMeter. All methods must be safe for concurrent use.
type Meter interface {
	// RecordResolve reports a provider resolve: its scheme, duration, and error
	// (nil on success).
	RecordResolve(scheme string, dur time.Duration, err error)
	// RecordRefresh reports that a watched value changed and was reconciled.
	RecordRefresh(scheme string)
	// RecordWatchError reports a provider watch-channel error.
	RecordWatchError(scheme string)
}

// Tracer is the minimal tracing sink mamori emits to (see Meter for the no-dep
// rationale). Pass one with WithTracer.
type Tracer interface {
	// StartResolve begins a span for a resolve and returns a derived context plus
	// a finish function to be called with the resolve error (nil on success).
	StartResolve(ctx context.Context, scheme, ref string) (context.Context, func(err error))
}

// noopMeter / noopTracer are the defaults, used when no observer is configured.
type noopMeter struct{}

func (noopMeter) RecordResolve(string, time.Duration, error) {}
func (noopMeter) RecordRefresh(string)                       {}
func (noopMeter) RecordWatchError(string)                    {}

type noopTracer struct{}

func (noopTracer) StartResolve(ctx context.Context, _, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}
