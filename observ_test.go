package mamori

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingMeter counts every Meter call, so a test can assert an engine event
// reached the metrics sink without needing a real backend.
type recordingMeter struct {
	mu            sync.Mutex
	resolves      int
	refreshes     int
	watchErrors   int
	stale         []string
	dropped       int
	applyRejected []RejectReason
}

func (m *recordingMeter) RecordResolve(string, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolves++
}
func (m *recordingMeter) RecordRefresh(string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshes++
}
func (m *recordingMeter) RecordWatchError(string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watchErrors++
}
func (m *recordingMeter) RecordStale(scheme string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stale = append(m.stale, scheme)
}
func (m *recordingMeter) RecordChangeDropped() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped++
}
func (m *recordingMeter) RecordApplyRejected(r RejectReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyRejected = append(m.applyRejected, r)
}

func (m *recordingMeter) droppedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}
func (m *recordingMeter) rejections() []RejectReason {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]RejectReason(nil), m.applyRejected...)
}
func (m *recordingMeter) staleSchemes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.stale...)
}

// TestMeterCountsDroppedChangeEvent is the counter this whole task exists for.
// enqueue silently discards the oldest event when the dispatch queue is full,
// and until #70 nothing recorded it at all; a log line is readable but not
// alertable.
func TestMeterCountsDroppedChangeEvent(t *testing.T) {
	m := &recordingMeter{}
	o := defaultOptions()
	WithMeter(m)(o)
	WithQueueDepth(1)(o)

	e := &engine[struct{}]{o: o, dispatch: make(chan Change[struct{}], 1)}
	e.enqueue(Change[struct{}]{})
	e.enqueue(Change[struct{}]{}) // overflows, drops the first

	if got := m.droppedCount(); got != 1 {
		t.Errorf("dropped count = %d, want 1", got)
	}
}

// TestMeterCountsValidationRejection covers buildCandidate's
// validator.Validate failure branch, mirrored from TestLogsValidationRejected
// in logging_test.go.
func TestMeterCountsValidationRejection(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("mval")
	wp.set("level", "info", "l1")
	m := &recordingMeter{}

	type Config struct {
		Level string `source:"mval://level" validate:"oneof=debug info warn error"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("level", "BOGUS", "l2") // fails oneof validation
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "one RejectValidation recorded", func() bool {
		return len(m.rejections()) == 1
	})

	got := m.rejections()
	if len(got) != 1 || got[0] != RejectValidation {
		t.Errorf("rejections = %v, want [%v]", got, RejectValidation)
	}
}

// TestMeterCountsPreApplyRejection covers flush's PreApply-gate rejection
// branch, mirrored from TestLogsPreApplyRejected in logging_test.go.
func TestMeterCountsPreApplyRejection(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("mpre")
	wp.set("a", "first", "v1")
	m := &recordingMeter{}
	reject := errors.New("credential does not work")

	type Config struct {
		A string `source:"mpre://a"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
		PreApply(func(_ context.Context, ev Change[Config]) error {
			if ev.New.A == "second" {
				return reject
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "one RejectPreApply recorded", func() bool {
		return len(m.rejections()) == 1
	})

	got := m.rejections()
	if len(got) != 1 || got[0] != RejectPreApply {
		t.Errorf("rejections = %v, want [%v]", got, RejectPreApply)
	}
}

// TestMeterCountsStale covers reportTerminalError's WithStale escalation
// branch, mirrored from TestLogsStaleValue in logging_test.go.
func TestMeterCountsStale(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("mst")
	wp.set("v", "initial", "v1")
	m := &recordingMeter{}

	type Config struct {
		V string `source:"mst://v"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithMeter(m),
		WithStale(time.Minute),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	clk.Advance(2 * time.Minute)
	wp.pushErr("v", errors.New("boom"))
	waitUntil(t, 2*time.Second, "stale scheme recorded", func() bool {
		return len(m.staleSchemes()) == 1
	})

	got := m.staleSchemes()
	if len(got) != 1 || got[0] != "mst" {
		t.Errorf("stale schemes = %v, want [mst]", got)
	}
}
