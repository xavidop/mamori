package mamoriotel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	mamoriotel "github.com/xavidop/mamori/x/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// collect builds a manual-reader-backed meter, runs fn against the adapter, and
// returns the collected metrics.
func collect(t *testing.T, fn func(m interface {
	RecordResolve(string, time.Duration, error)
	RecordRefresh(string)
	RecordWatchError(string)
})) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	m, err := mamoriotel.NewMeter(provider.Meter("mamori-test"))
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}

	fn(m)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not found in collected metrics", name)
	return metricdata.Metrics{}
}

func attrString(t *testing.T, set attribute.Set, key string) string {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		t.Fatalf("attribute %q not present in set %v", key, set.Encoded(attribute.DefaultEncoder()))
	}
	return v.AsString()
}

func TestMeter_RecordResolve(t *testing.T) {
	resolveErr := errors.New("boom")

	rm := collect(t, func(m interface {
		RecordResolve(string, time.Duration, error)
		RecordRefresh(string)
		RecordWatchError(string)
	}) {
		m.RecordResolve("file", 5*time.Millisecond, nil)
		m.RecordResolve("aws", 20*time.Millisecond, resolveErr)
	})

	metric := findMetric(t, rm, mamoriotel.MetricResolveDuration)
	if metric.Unit != "ms" {
		t.Errorf("resolve duration unit = %q, want %q", metric.Unit, "ms")
	}

	hist, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("resolve duration data has type %T, want Histogram[float64]", metric.Data)
	}
	if len(hist.DataPoints) != 2 {
		t.Fatalf("resolve duration data points = %d, want 2", len(hist.DataPoints))
	}

	// Index the data points by (scheme, status) so we can assert on each.
	got := map[string]metricdata.HistogramDataPoint[float64]{}
	for _, dp := range hist.DataPoints {
		key := attrString(t, dp.Attributes, "scheme") + "/" + attrString(t, dp.Attributes, "status")
		got[key] = dp
	}

	okDP, present := got["file/ok"]
	if !present {
		t.Fatalf("no data point for file/ok; got keys %v", keys(got))
	}
	if okDP.Count != 1 {
		t.Errorf("file/ok count = %d, want 1", okDP.Count)
	}
	if okDP.Sum != 5 {
		t.Errorf("file/ok sum = %v ms, want 5", okDP.Sum)
	}

	errDP, present := got["aws/error"]
	if !present {
		t.Fatalf("no data point for aws/error; got keys %v", keys(got))
	}
	if errDP.Count != 1 {
		t.Errorf("aws/error count = %d, want 1", errDP.Count)
	}
	if errDP.Sum != 20 {
		t.Errorf("aws/error sum = %v ms, want 20", errDP.Sum)
	}
}

func TestMeter_RecordRefreshAndWatchError(t *testing.T) {
	rm := collect(t, func(m interface {
		RecordResolve(string, time.Duration, error)
		RecordRefresh(string)
		RecordWatchError(string)
	}) {
		m.RecordRefresh("file")
		m.RecordRefresh("file")
		m.RecordWatchError("aws")
	})

	refresh := findMetric(t, rm, mamoriotel.MetricRefreshCount)
	refreshSum, ok := refresh.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("refresh count data has type %T, want Sum[int64]", refresh.Data)
	}
	if len(refreshSum.DataPoints) != 1 {
		t.Fatalf("refresh count data points = %d, want 1", len(refreshSum.DataPoints))
	}
	if got := refreshSum.DataPoints[0].Value; got != 2 {
		t.Errorf("refresh count = %d, want 2", got)
	}
	if s := attrString(t, refreshSum.DataPoints[0].Attributes, "scheme"); s != "file" {
		t.Errorf("refresh scheme = %q, want %q", s, "file")
	}

	watch := findMetric(t, rm, mamoriotel.MetricWatchErrors)
	watchSum, ok := watch.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("watch errors data has type %T, want Sum[int64]", watch.Data)
	}
	if len(watchSum.DataPoints) != 1 {
		t.Fatalf("watch errors data points = %d, want 1", len(watchSum.DataPoints))
	}
	if got := watchSum.DataPoints[0].Value; got != 1 {
		t.Errorf("watch errors count = %d, want 1", got)
	}
	if s := attrString(t, watchSum.DataPoints[0].Attributes, "scheme"); s != "aws" {
		t.Errorf("watch errors scheme = %q, want %q", s, "aws")
	}
}

func TestTracer_StartResolveSuccess(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tr := mamoriotel.NewTracer(provider.Tracer("mamori-test"))

	ctx, finish := tr.StartResolve(context.Background(), "file", "file:///etc/app.yaml#port")
	if ctx == nil {
		t.Fatal("StartResolve returned nil context")
	}
	finish(nil)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != mamoriotel.SpanResolve {
		t.Errorf("span name = %q, want %q", span.Name(), mamoriotel.SpanResolve)
	}
	assertSpanAttr(t, span.Attributes(), "mamori.scheme", "file")
	assertSpanAttr(t, span.Attributes(), "mamori.ref", "file:///etc/app.yaml#port")

	if span.Status().Code != codes.Ok {
		t.Errorf("span status code = %v, want Ok", span.Status().Code)
	}
	if n := len(span.Events()); n != 0 {
		t.Errorf("success span recorded %d events, want 0", n)
	}
}

func TestTracer_StartResolveError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tr := mamoriotel.NewTracer(provider.Tracer("mamori-test"))

	_, finish := tr.StartResolve(context.Background(), "aws", "aws://param/db")
	finish(errors.New("access denied"))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]

	if span.Status().Code != codes.Error {
		t.Errorf("span status code = %v, want Error", span.Status().Code)
	}
	if span.Status().Description != "access denied" {
		t.Errorf("span status description = %q, want %q", span.Status().Description, "access denied")
	}

	// RecordError adds an "exception" span event.
	events := span.Events()
	if len(events) != 1 {
		t.Fatalf("error span events = %d, want 1", len(events))
	}
	if events[0].Name != "exception" {
		t.Errorf("event name = %q, want %q", events[0].Name, "exception")
	}
}

func assertSpanAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			if got := kv.Value.AsString(); got != want {
				t.Errorf("span attribute %q = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("span attribute %q not found", key)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
