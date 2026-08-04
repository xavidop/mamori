package prom

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/xavidop/mamori"
)

func TestRecordsResolveWithStatusAndKind(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RecordResolve("aws-sm", 5*time.Millisecond, nil)
	m.RecordResolve("aws-sm", 5*time.Millisecond, mamori.ErrNotFound)

	got, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if got == 0 {
		t.Fatal("no metrics were gathered")
	}
}

// TestRejectReasonIsABoundedLabel is the cardinality guard. A metrics backend
// dies from unbounded label values, so only the reasons mamori.RejectReason
// actually defines - validation, preapply, and (since WithDerive) derive -
// can ever appear.
func TestRejectReasonIsABoundedLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RecordApplyRejected(mamori.RejectValidation)
	m.RecordApplyRejected(mamori.RejectPreApply)
	m.RecordApplyRejected(mamori.RejectDerive)

	// CollectAndFormat only includes the metric families named by
	// metricNames (see filterMetrics in the testutil package: an empty list
	// filters everything out, not nothing), so name the family explicitly
	// rather than relying on a bare two-argument call to return everything.
	out, err := testutil.CollectAndFormat(reg, expfmt.TypeTextPlain, MetricApplyRejectedTotal)
	if err != nil {
		t.Fatalf("CollectAndFormat: %v", err)
	}
	s := string(out)
	for _, want := range []string{`reason="validation"`, `reason="preapply"`, `reason="derive"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
}

// TestNoRefOrValueEverBecomesALabel guards the property that matters most: a
// label carrying a ref would be both a cardinality bomb and a credential leak.
func TestNoRefOrValueEverBecomesALabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RecordResolve("aws-sm", time.Millisecond, errors.New("boom"))
	m.RecordStale("aws-sm")
	m.RecordChangeDropped()

	// Name every family that was just recorded to, for the same reason as
	// TestRejectReasonIsABoundedLabel: CollectAndFormat's metricNames filter
	// keeps only what is named, and drops everything if nothing is named.
	out, err := testutil.CollectAndFormat(reg, expfmt.TypeTextPlain,
		MetricResolveDuration, MetricStaleTotal, MetricChangeDroppedTotal)
	if err != nil {
		t.Fatalf("CollectAndFormat: %v", err)
	}
	s := string(out)
	for _, forbidden := range []string{"ref=", "value=", "://"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("metric output contains %q:\n%s", forbidden, out)
		}
	}
}

// TestNewReturnsErrorOnDuplicateRegistration confirms New reports registration
// failure as an error, matching x/otel's NewMeter, rather than panicking: a
// caller who accidentally calls New twice against the same registry gets an
// error back, not a crash.
func TestNewReturnsErrorOnDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := New(reg); err == nil {
		t.Fatal("second New against the same registry: got nil error, want a registration error")
	}
}

// TestRecordChangeDroppedHasNoLabels confirms the dropped-change counter is a
// bare Counter with no labels: it is a process-wide property of the dispatch
// queue, not a per-scheme one.
func TestRecordChangeDroppedHasNoLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RecordChangeDropped()
	m.RecordChangeDropped()
	m.RecordChangeDropped()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, fam := range families {
		if fam.GetName() != MetricChangeDroppedTotal {
			continue
		}
		found = true
		if n := len(fam.Metric); n != 1 {
			t.Fatalf("%s metric count = %d, want 1", MetricChangeDroppedTotal, n)
		}
		metric := fam.Metric[0]
		if n := len(metric.GetLabel()); n != 0 {
			t.Errorf("%s labels = %d, want 0", MetricChangeDroppedTotal, n)
		}
		if got := metric.GetCounter().GetValue(); got != 3 {
			t.Errorf("%s value = %v, want 3", MetricChangeDroppedTotal, got)
		}
	}
	if !found {
		t.Fatalf("metric %s not found", MetricChangeDroppedTotal)
	}
}

// TestRecordBootstrapWriteFailedHasNoLabels confirms the
// bootstrap-write-failure counter is a bare Counter: there is one snapshot
// file per process, so there is nothing to break it down by.
func TestRecordBootstrapWriteFailedHasNoLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RecordBootstrapWriteFailed()
	m.RecordBootstrapWriteFailed()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, fam := range families {
		if fam.GetName() != MetricBootstrapWriteFailedTotal {
			continue
		}
		found = true
		if n := len(fam.Metric); n != 1 {
			t.Fatalf("%s metric count = %d, want 1", MetricBootstrapWriteFailedTotal, n)
		}
		metric := fam.Metric[0]
		if n := len(metric.GetLabel()); n != 0 {
			t.Errorf("%s labels = %d, want 0", MetricBootstrapWriteFailedTotal, n)
		}
		if got := metric.GetCounter().GetValue(); got != 2 {
			t.Errorf("%s value = %v, want 2", MetricBootstrapWriteFailedTotal, got)
		}
	}
	if !found {
		t.Fatalf("metric %s not found", MetricBootstrapWriteFailedTotal)
	}
}
