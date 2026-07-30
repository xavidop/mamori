// Package prom implements mamori.Meter directly against
// github.com/prometheus/client_golang, for shops that run Prometheus without
// adopting OpenTelemetry. If you already use OTel, prefer x/otel with a
// Prometheus exporter instead of this module; this exists for the case where
// client_golang is the only metrics dependency you want in your build.
//
// Usage:
//
//	import (
//	    "github.com/prometheus/client_golang/prometheus"
//	    "github.com/xavidop/mamori"
//	    mamoriprom "github.com/xavidop/mamori/x/prom"
//	)
//
//	reg := prometheus.NewRegistry()
//	m, err := mamoriprom.New(reg)
//	if err != nil {
//	    return err
//	}
//
//	cfg, err := mamori.Load[Config](ctx, mamori.WithMeter(m))
package prom

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/xavidop/mamori"
)

// Instrument names registered by New. Names take a "mamori_" prefix and the
// Prometheus suffix convention: "_seconds" for a duration histogram, "_total"
// for counters.
const (
	// MetricResolveDuration is a histogram (unit: seconds) of provider resolve
	// latency, labeled "scheme", "status" (ok|error), and "error_kind" (see
	// New). Prometheus convention measures durations in seconds; this differs
	// from x/otel, which records the same event in milliseconds, so the same
	// resolve reads differently on the two backends.
	MetricResolveDuration = "mamori_resolve_duration_seconds"
	// MetricRefreshTotal is a counter of reconciled value changes, labeled
	// "scheme".
	MetricRefreshTotal = "mamori_refresh_total"
	// MetricWatchErrorsTotal is a counter of provider watch-channel errors,
	// labeled "scheme".
	MetricWatchErrorsTotal = "mamori_watch_errors_total"
	// MetricStaleTotal is a counter of values that have gone unrefreshed past
	// the WithStale threshold, labeled "scheme".
	MetricStaleTotal = "mamori_stale_total"
	// MetricChangeDroppedTotal is a counter of OnChange dispatch events
	// discarded because the queue was full. It carries no labels: it is a
	// process-wide property of the dispatch queue, not a per-scheme one. A
	// non-zero rate means an OnChange handler is not keeping up with the rate
	// of applied changes.
	MetricChangeDroppedTotal = "mamori_change_dropped_total"
	// MetricApplyRejectedTotal is a counter of candidate configurations
	// refused before being applied, labeled "reason" (see mamori.RejectReason:
	// "validation" or "preapply").
	MetricApplyRejectedTotal = "mamori_apply_rejected_total"
)

// Metric label names.
//
// Every one of these is drawn from a small, closed set: a provider scheme, a
// fixed ok/error status, a mamori.Kind, or a mamori.RejectReason. A ref or a
// resolved value must never become a label: a ref can carry an inline
// credential (a "?token=" query option), and both are unbounded, which is
// exactly what a metric label must never be. An unbounded label is a
// cardinality bomb for whatever scrapes this registry, and, in the ref case,
// a credential leak on top of it.
const (
	labelScheme    = "scheme"
	labelStatus    = "status"
	labelErrorKind = "error_kind"
	labelReason    = "reason"

	statusOK    = "ok"
	statusError = "error"
)

// meter implements mamori.Meter on top of Prometheus client_golang
// instruments registered against a single prometheus.Registerer.
//
// It is safe for concurrent use: every underlying Prometheus instrument is
// concurrency-safe, and the struct is immutable after construction.
type meter struct {
	resolveDuration    *prometheus.HistogramVec
	refreshTotal       *prometheus.CounterVec
	watchErrorsTotal   *prometheus.CounterVec
	staleTotal         *prometheus.CounterVec
	changeDroppedTotal prometheus.Counter
	applyRejectedTotal *prometheus.CounterVec
}

// New builds a mamori.Meter backed by Prometheus client_golang, creating and
// registering six instruments against reg up front:
//
//   - a HistogramVec "mamori_resolve_duration_seconds" recording resolve
//     latency in seconds, labeled "scheme", "status" (ok|error), and
//     "error_kind" (mamori.ErrorKind(err), the empty string on success since
//     every series in a HistogramVec must share the same label set);
//   - a CounterVec "mamori_refresh_total" labeled "scheme";
//   - a CounterVec "mamori_watch_errors_total" labeled "scheme";
//   - a CounterVec "mamori_stale_total" labeled "scheme";
//   - a bare Counter "mamori_change_dropped_total", with no labels;
//   - a CounterVec "mamori_apply_rejected_total" labeled "reason".
//
// An error is returned (rather than a panic) if any instrument cannot be
// registered against reg, so a caller registering twice, or against a
// registry that already has a colliding collector, gets an error instead of
// a crash.
func New(reg prometheus.Registerer) (mamori.Meter, error) {
	resolveDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: MetricResolveDuration,
		Help: "Duration of mamori provider resolves, in seconds.",
	}, []string{labelScheme, labelStatus, labelErrorKind})
	if err := reg.Register(resolveDuration); err != nil {
		return nil, err
	}

	refreshTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricRefreshTotal,
		Help: "Number of mamori watched-value refreshes reconciled.",
	}, []string{labelScheme})
	if err := reg.Register(refreshTotal); err != nil {
		return nil, err
	}

	watchErrorsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricWatchErrorsTotal,
		Help: "Number of mamori provider watch-channel errors.",
	}, []string{labelScheme})
	if err := reg.Register(watchErrorsTotal); err != nil {
		return nil, err
	}

	staleTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricStaleTotal,
		Help: "Number of mamori watched values that exceeded the WithStale threshold.",
	}, []string{labelScheme})
	if err := reg.Register(staleTotal); err != nil {
		return nil, err
	}

	changeDroppedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: MetricChangeDroppedTotal,
		Help: "Number of mamori OnChange dispatch events dropped because the queue was full.",
	})
	if err := reg.Register(changeDroppedTotal); err != nil {
		return nil, err
	}

	applyRejectedTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricApplyRejectedTotal,
		Help: "Number of mamori candidate configurations rejected before being applied.",
	}, []string{labelReason})
	if err := reg.Register(applyRejectedTotal); err != nil {
		return nil, err
	}

	return &meter{
		resolveDuration:    resolveDuration,
		refreshTotal:       refreshTotal,
		watchErrorsTotal:   watchErrorsTotal,
		staleTotal:         staleTotal,
		changeDroppedTotal: changeDroppedTotal,
		applyRejectedTotal: applyRejectedTotal,
	}, nil
}

// RecordResolve records the resolve duration, in seconds, labeled with the
// scheme and a status of "ok" or "error". A failed resolve additionally
// carries error_kind, the same classification mamori.ErrorKind(err) reports;
// a successful resolve carries the empty string for error_kind rather than
// omitting the label, since a Prometheus HistogramVec requires every series
// to share the same label set.
func (m *meter) RecordResolve(scheme string, dur time.Duration, err error) {
	status := statusOK
	var kind string
	if err != nil {
		status = statusError
		kind = string(mamori.ErrorKind(err))
	}
	m.resolveDuration.WithLabelValues(scheme, status, kind).Observe(dur.Seconds())
}

// RecordRefresh increments the refresh counter for the scheme.
func (m *meter) RecordRefresh(scheme string) {
	m.refreshTotal.WithLabelValues(scheme).Inc()
}

// RecordWatchError increments the watch-error counter for the scheme.
func (m *meter) RecordWatchError(scheme string) {
	m.watchErrorsTotal.WithLabelValues(scheme).Inc()
}

// RecordStale increments the stale-value counter for the scheme.
func (m *meter) RecordStale(scheme string) {
	m.staleTotal.WithLabelValues(scheme).Inc()
}

// RecordChangeDropped increments the dropped-change-event counter. It carries
// no labels: the dispatch queue it reports on is process-wide, not
// per-scheme.
func (m *meter) RecordChangeDropped() {
	m.changeDroppedTotal.Inc()
}

// RecordApplyRejected increments the rejected-apply counter, labeled with
// reason ("validation" or "preapply").
func (m *meter) RecordApplyRejected(reason mamori.RejectReason) {
	m.applyRejectedTotal.WithLabelValues(string(reason)).Inc()
}
