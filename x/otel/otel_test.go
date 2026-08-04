package mamoriotel_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xavidop/mamori"
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
func collect(t *testing.T, fn func(m mamori.Meter)) metricdata.ResourceMetrics {
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

	rm := collect(t, func(m mamori.Meter) {
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
	rm := collect(t, func(m mamori.Meter) {
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

func TestMeter_RecordStale(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordStale("file")
		m.RecordStale("file")
	})

	stale := findMetric(t, rm, mamoriotel.MetricStaleCount)
	staleSum, ok := stale.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("stale count data has type %T, want Sum[int64]", stale.Data)
	}
	if len(staleSum.DataPoints) != 1 {
		t.Fatalf("stale count data points = %d, want 1", len(staleSum.DataPoints))
	}
	if got := staleSum.DataPoints[0].Value; got != 2 {
		t.Errorf("stale count = %d, want 2", got)
	}
	if s := attrString(t, staleSum.DataPoints[0].Attributes, "scheme"); s != "file" {
		t.Errorf("stale scheme = %q, want %q", s, "file")
	}
}

// TestMeter_RecordChangeDropped confirms the dropped-change-event counter
// carries no attributes: it is a process-wide property of the dispatch queue,
// not a per-scheme one.
func TestMeter_RecordChangeDropped(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordChangeDropped()
		m.RecordChangeDropped()
		m.RecordChangeDropped()
	})

	dropped := findMetric(t, rm, mamoriotel.MetricChangeDroppedCount)
	droppedSum, ok := dropped.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("change dropped data has type %T, want Sum[int64]", dropped.Data)
	}
	if len(droppedSum.DataPoints) != 1 {
		t.Fatalf("change dropped data points = %d, want 1", len(droppedSum.DataPoints))
	}
	if got := droppedSum.DataPoints[0].Value; got != 3 {
		t.Errorf("change dropped count = %d, want 3", got)
	}
	if n := droppedSum.DataPoints[0].Attributes.Len(); n != 0 {
		t.Errorf("change dropped attributes = %d, want 0", n)
	}
}

// TestMeter_RecordApplyRejected confirms the rejected-apply counter is tagged
// with reason, and that mamori.RejectValidation, mamori.RejectPreApply, and
// mamori.RejectDerive each produce their own distinct data point.
func TestMeter_RecordApplyRejected(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordApplyRejected(mamori.RejectValidation)
		m.RecordApplyRejected(mamori.RejectPreApply)
		m.RecordApplyRejected(mamori.RejectPreApply)
		m.RecordApplyRejected(mamori.RejectDerive)
	})

	rejected := findMetric(t, rm, mamoriotel.MetricApplyRejectedCount)
	rejectedSum, ok := rejected.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("apply rejected data has type %T, want Sum[int64]", rejected.Data)
	}
	if len(rejectedSum.DataPoints) != 3 {
		t.Fatalf("apply rejected data points = %d, want 3", len(rejectedSum.DataPoints))
	}

	got := map[string]int64{}
	for _, dp := range rejectedSum.DataPoints {
		got[attrString(t, dp.Attributes, "reason")] = dp.Value
	}
	if got["validation"] != 1 {
		t.Errorf("apply rejected[validation] = %d, want 1", got["validation"])
	}
	if got["preapply"] != 2 {
		t.Errorf("apply rejected[preapply] = %d, want 2", got["preapply"])
	}
	if got["derive"] != 1 {
		t.Errorf("apply rejected[derive] = %d, want 1", got["derive"])
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

// TestMeter_RecordResolveErrorKind confirms a failed resolve tags the
// resolve-duration histogram with mamori.error.kind, classified via
// mamori.ErrorKind, so a dashboard can split a denied permission from a
// throttled request without parsing error strings.
func TestMeter_RecordResolveErrorKind(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordResolve("aws-sm", 5*time.Millisecond, fmt.Errorf("%w: denied by policy", mamori.ErrPermissionDenied))
	})

	metric := findMetric(t, rm, mamoriotel.MetricResolveDuration)
	hist, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("resolve duration data has type %T, want Histogram[float64]", metric.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("resolve duration data points = %d, want 1", len(hist.DataPoints))
	}

	if got := attrString(t, hist.DataPoints[0].Attributes, "mamori.error.kind"); got != string(mamori.KindPermissionDenied) {
		t.Errorf("mamori.error.kind = %q, want %q", got, mamori.KindPermissionDenied)
	}
}

// TestMeter_RecordResolveOmitsErrorKindOnSuccess confirms a successful resolve
// carries no mamori.error.kind attribute at all, rather than an empty or
// placeholder value, so the attribute's mere presence selects failures.
func TestMeter_RecordResolveOmitsErrorKindOnSuccess(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordResolve("env", time.Millisecond, nil)
	})

	metric := findMetric(t, rm, mamoriotel.MetricResolveDuration)
	hist, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("resolve duration data has type %T, want Histogram[float64]", metric.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("resolve duration data points = %d, want 1", len(hist.DataPoints))
	}

	if _, found := hist.DataPoints[0].Attributes.Value(attribute.Key("mamori.error.kind")); found {
		t.Errorf("successful resolve carried a mamori.error.kind attribute, want it absent")
	}
}

// TestTracer_StartResolveErrorCarriesErrorKind confirms a failed resolve span
// carries the same mamori.error.kind classification as the histogram.
func TestTracer_StartResolveErrorCarriesErrorKind(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tr := mamoriotel.NewTracer(provider.Tracer("mamori-test"))

	_, finish := tr.StartResolve(context.Background(), "aws-sm", "aws-sm://db/password")
	finish(fmt.Errorf("%w: denied by policy", mamori.ErrPermissionDenied))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	assertSpanAttr(t, spans[0].Attributes(), "mamori.error.kind", string(mamori.KindPermissionDenied))
}

// TestTracer_StartResolveOmitsErrorKindOnSuccess confirms a successful resolve
// span carries no mamori.error.kind attribute.
func TestTracer_StartResolveOmitsErrorKindOnSuccess(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tr := mamoriotel.NewTracer(provider.Tracer("mamori-test"))

	_, finish := tr.StartResolve(context.Background(), "file", "file:///etc/app.yaml#port")
	finish(nil)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	assertSpanAttrAbsent(t, spans[0].Attributes(), "mamori.error.kind")
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

// assertSpanAttrAbsent fails if key is present among attrs. It is the inverse
// of assertSpanAttr, used to confirm a successful resolve carries no error
// classification.
func assertSpanAttrAbsent(t *testing.T, attrs []attribute.KeyValue, key string) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			t.Errorf("span attribute %q = %q, want absent", key, kv.Value.AsString())
			return
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMeter_RecordBootstrapWriteFailed confirms the bootstrap-write-failure
// counter carries no attributes: there is one snapshot file per process, so
// there is nothing to break it down by.
func TestMeter_RecordBootstrapWriteFailed(t *testing.T) {
	rm := collect(t, func(m mamori.Meter) {
		m.RecordBootstrapWriteFailed()
		m.RecordBootstrapWriteFailed()
	})

	failed := findMetric(t, rm, mamoriotel.MetricBootstrapWriteFailedCount)
	failedSum, ok := failed.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("bootstrap write failed data has type %T, want Sum[int64]", failed.Data)
	}
	if len(failedSum.DataPoints) != 1 {
		t.Fatalf("bootstrap write failed data points = %d, want 1", len(failedSum.DataPoints))
	}
	if got := failedSum.DataPoints[0].Value; got != 2 {
		t.Errorf("bootstrap write failed count = %d, want 2", got)
	}
	if n := failedSum.DataPoints[0].Attributes.Len(); n != 0 {
		t.Errorf("bootstrap write failed attributes = %d, want 0", n)
	}
}
