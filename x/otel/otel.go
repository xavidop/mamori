// Package mamoriotel is the OpenTelemetry bridge for the mamori configuration
// library. It adapts an OpenTelemetry metric.Meter and trace.Tracer to the
// minimal mamori.Meter and mamori.Tracer interfaces, so that mamori can emit
// metrics and spans without the core module taking any OpenTelemetry dependency.
//
// Usage:
//
//	import (
//	    "go.opentelemetry.io/otel"
//	    "github.com/xavidop/mamori"
//	    mamoriotel "github.com/xavidop/mamori/x/otel"
//	)
//
//	m, err := mamoriotel.NewMeter(otel.Meter("mamori"))
//	if err != nil {
//	    return err
//	}
//	tr := mamoriotel.NewTracer(otel.Tracer("mamori"))
//
//	cfg, err := mamori.Load[Config](ctx,
//	    mamori.WithMeter(m),
//	    mamori.WithTracer(tr),
//	)
//
// The package is named mamoriotel (rather than otel) to avoid clashing with
// go.opentelemetry.io/otel when both are imported together.
package mamoriotel

import (
	"context"
	"time"

	"github.com/xavidop/mamori"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instrument names emitted by the meter adapter.
const (
	// MetricResolveDuration is a histogram (unit: ms) of provider resolve
	// latency, with attributes "scheme" and "status" (ok|error).
	MetricResolveDuration = "mamori.resolve.duration"
	// MetricRefreshCount is a counter of reconciled value changes, with
	// attribute "scheme".
	MetricRefreshCount = "mamori.refresh.count"
	// MetricWatchErrors is a counter of provider watch-channel errors, with
	// attribute "scheme".
	MetricWatchErrors = "mamori.watch.errors"
)

// Metric attribute keys.
const (
	attrScheme = "scheme"
	attrStatus = "status"

	statusOK    = "ok"
	statusError = "error"
)

// Span name and attribute keys emitted by the tracer adapter.
const (
	// SpanResolve is the name of the span started for each resolve.
	SpanResolve = "mamori.resolve"

	spanAttrScheme = "mamori.scheme"
	spanAttrRef    = "mamori.ref"
)

// meter implements mamori.Meter on top of an OpenTelemetry metric.Meter.
//
// It is safe for concurrent use: the underlying OTel instruments are
// concurrency-safe and the struct is immutable after construction.
type meter struct {
	ctx             context.Context
	resolveDuration metric.Float64Histogram
	refreshCount    metric.Int64Counter
	watchErrors     metric.Int64Counter
}

// NewMeter builds a mamori.Meter backed by the given OpenTelemetry meter. It
// creates three instruments up front:
//
//   - a Float64Histogram "mamori.resolve.duration" (unit ms) recording resolve
//     latency, tagged with "scheme" and "status" (ok|error);
//   - an Int64Counter "mamori.refresh.count" tagged with "scheme";
//   - an Int64Counter "mamori.watch.errors" tagged with "scheme".
//
// An error is returned if any instrument cannot be created. The returned Meter
// records measurements against context.Background(); pass it to
// mamori.WithMeter.
func NewMeter(m metric.Meter) (mamori.Meter, error) {
	resolveDuration, err := m.Float64Histogram(
		MetricResolveDuration,
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of mamori provider resolves, in milliseconds."),
	)
	if err != nil {
		return nil, err
	}

	refreshCount, err := m.Int64Counter(
		MetricRefreshCount,
		metric.WithDescription("Number of mamori watched-value refreshes reconciled."),
	)
	if err != nil {
		return nil, err
	}

	watchErrors, err := m.Int64Counter(
		MetricWatchErrors,
		metric.WithDescription("Number of mamori provider watch-channel errors."),
	)
	if err != nil {
		return nil, err
	}

	return &meter{
		ctx:             context.Background(),
		resolveDuration: resolveDuration,
		refreshCount:    refreshCount,
		watchErrors:     watchErrors,
	}, nil
}

// RecordResolve records the resolve duration (in milliseconds) tagged with the
// scheme and a status of "ok" or "error".
func (m *meter) RecordResolve(scheme string, dur time.Duration, err error) {
	status := statusOK
	if err != nil {
		status = statusError
	}
	m.resolveDuration.Record(
		m.ctx,
		float64(dur)/float64(time.Millisecond),
		metric.WithAttributes(
			attribute.String(attrScheme, scheme),
			attribute.String(attrStatus, status),
		),
	)
}

// RecordRefresh increments the refresh counter for the scheme.
func (m *meter) RecordRefresh(scheme string) {
	m.refreshCount.Add(m.ctx, 1, metric.WithAttributes(attribute.String(attrScheme, scheme)))
}

// RecordWatchError increments the watch-error counter for the scheme.
func (m *meter) RecordWatchError(scheme string) {
	m.watchErrors.Add(m.ctx, 1, metric.WithAttributes(attribute.String(attrScheme, scheme)))
}

// tracer implements mamori.Tracer on top of an OpenTelemetry trace.Tracer.
type tracer struct {
	t trace.Tracer
}

// NewTracer builds a mamori.Tracer backed by the given OpenTelemetry tracer.
// Pass the result to mamori.WithTracer.
func NewTracer(t trace.Tracer) mamori.Tracer {
	return &tracer{t: t}
}

// StartResolve starts a span named "mamori.resolve" with attributes
// "mamori.scheme" and "mamori.ref". The returned finish function ends the span:
// on a non-nil error it records the error and sets the span status to
// codes.Error; otherwise it sets codes.Ok.
func (tr *tracer) StartResolve(ctx context.Context, scheme, ref string) (context.Context, func(err error)) {
	ctx, span := tr.t.Start(
		ctx,
		SpanResolve,
		trace.WithAttributes(
			attribute.String(spanAttrScheme, scheme),
			attribute.String(spanAttrRef, ref),
		),
	)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}
