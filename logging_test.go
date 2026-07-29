package mamori

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHandler captures every record written to it, so tests can assert on
// level, message, and attributes without parsing formatted output.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   []slog.Attr
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Record.Attrs is only iterable once per record in some handlers, so clone
	// the record to keep it replayable across multiple assertions.
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &recordingHandler{attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record{}, h.records...)
}

// find returns the first record whose message is msg, or false.
func (h *recordingHandler) find(msg string) (slog.Record, bool) {
	for _, r := range h.all() {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// dump renders every record, message and attributes, as one string. It is what
// the secret-safety tests scan.
func (h *recordingHandler) dump() string {
	var b strings.Builder
	for _, r := range h.all() {
		b.WriteString(r.Level.String())
		b.WriteByte(' ')
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteByte(' ')
			b.WriteString(a.Key)
			b.WriteByte('=')
			b.WriteString(a.Value.String())
			return true
		})
		b.WriteByte('\n')
	}
	return b.String()
}

func attrOf(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.String(), true
			return false
		}
		return true
	})
	return out, found
}

func newRecorder() (*recordingHandler, *slog.Logger) {
	h := &recordingHandler{}
	return h, slog.New(h)
}

// TestLoggerDefaultsToSilent is the property that makes this safe to add to a
// library: linking mamori in must not write to anyone's stderr.
func TestLoggerDefaultsToSilent(t *testing.T) {
	o := defaultOptions()
	if o.logger == nil {
		t.Fatal("defaultOptions left logger nil; every call site would panic")
	}
	if o.logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("the default logger is enabled; a library must stay silent until asked")
	}
}

// TestWithLoggerNilResetsToSilent covers the conditional-wiring case, where a
// caller passes nil for the off branch.
func TestWithLoggerNilResetsToSilent(t *testing.T) {
	o := defaultOptions()
	WithLogger(nil)(o)
	if o.logger == nil {
		t.Fatal("WithLogger(nil) left logger nil")
	}
	if o.logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("WithLogger(nil) produced an enabled logger")
	}
}

// TestLoggerNeverLogsAValue is the load-bearing safety property. A config log
// is precisely the artifact most likely to be shipped off the host, so a
// resolved value must never reach one. This drives a real Load with a provider
// returning a known sentinel and scans everything written at any level.
func TestLoggerNeverLogsAValue(t *testing.T) {
	const sentinel = "s3cr3t-sentinel-value-do-not-log"

	h, logger := newRecorder()
	type Config struct {
		Password string `source:"lgv://pw"`
	}
	p := &errAfterProvider{scheme: "lgv", val: Value{Bytes: []byte(sentinel), Version: "v1"}}
	p.ok.Store(1)
	_, err := Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := h.dump(); strings.Contains(got, sentinel) {
		t.Fatalf("a resolved value reached the log:\n%s", got)
	}
}

// TestLoggerRedactsRefCredentials covers the other leak path: a ref carrying an
// inline credential as a query option.
func TestLoggerRedactsRefCredentials(t *testing.T) {
	h, logger := newRecorder()
	type Config struct {
		V string `source:"lgr://thing?token=hunter2"`
	}
	// A failing provider guarantees the ref reaches a log line.
	p := &errAfterProvider{scheme: "lgr", fail: errors.New("boom")}
	_, _ = Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(p),
	)
	got := h.dump()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("an inline credential reached the log:\n%s", got)
	}
	if !strings.Contains(got, "token=") {
		t.Fatalf("expected the redacted ref to still name the option:\n%s", got)
	}
}

// TestLogsResolveFailure covers resolveRef's failure log on the one-shot Load
// path.
func TestLogsResolveFailure(t *testing.T) {
	h, logger := newRecorder()
	type Config struct {
		V string `source:"lgf://thing"`
	}
	p := &errAfterProvider{scheme: "lgf", fail: errors.New("boom")}
	_, _ = Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(p),
	)
	r, ok := h.find("resolve failed")
	if !ok {
		t.Fatalf("no 'resolve failed' record:\n%s", h.dump())
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn: the field still serves its last good value, so this is not an outage", r.Level)
	}
	if got, _ := attrOf(r, logAttrScheme); got != "lgf" {
		t.Errorf("scheme = %q, want %q", got, "lgf")
	}
	if got, _ := attrOf(r, logAttrKind); got == "" {
		t.Error("no kind attribute; an operator cannot filter by classification")
	}
}

// TestLogsWatchError covers reportTerminalError's unconditional "watch error"
// log, driven by a native-watch provider delivering a non-not-found error
// update at runtime.
func TestLogsWatchError(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lwe")
	wp.set("v", "initial", "v1")
	h, logger := newRecorder()

	type Config struct {
		V string `source:"lwe://v"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.pushErr("v", errors.New("boom"))
	waitUntil(t, 2*time.Second, "'watch error' record", func() bool {
		_, ok := h.find("watch error")
		return ok
	})

	r, _ := h.find("watch error")
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", r.Level)
	}
	if got, _ := attrOf(r, logAttrScheme); got != "lwe" {
		t.Errorf("scheme = %q, want lwe", got)
	}
}

// TestLogsStaleValue covers the WithStale escalation inside reportTerminalError:
// once a field has gone unrefreshed past its stale threshold, a further watch
// error also logs "value is stale" in addition to "watch error".
func TestLogsStaleValue(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lst")
	wp.set("v", "initial", "v1")
	h, logger := newRecorder()

	type Config struct {
		V string `source:"lst://v"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
		WithStale(time.Minute),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	clk.Advance(2 * time.Minute)
	wp.pushErr("v", errors.New("boom"))
	waitUntil(t, 2*time.Second, "'value is stale' record", func() bool {
		_, ok := h.find("value is stale")
		return ok
	})

	r, _ := h.find("value is stale")
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", r.Level)
	}
	if got, _ := attrOf(r, logAttrField); got != "V" {
		t.Errorf("field = %q, want V", got)
	}
}

// TestLogsResolveRecovered covers the "resolve recovered" log at
// handleChainNotFound's tolerated-absence branch: a field with a default: tag
// that first fails with a genuine (non-not-found) terminal error, then later
// resolves to a tolerated not-found, clears a real prior error and logs the
// recovery.
func TestLogsResolveRecovered(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lrec")
	wp.set("v", "initial", "v1")
	h, logger := newRecorder()

	type Config struct {
		V string `source:"lrec://v" default:"fallback"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.pushErr("v", errors.New("permission denied"))
	waitUntil(t, 2*time.Second, "'watch error' record", func() bool {
		_, ok := h.find("watch error")
		return ok
	})

	wp.pushErr("v", ErrNotFound) // tolerated (default: tag) -> recovers
	waitUntil(t, 2*time.Second, "'resolve recovered' record", func() bool {
		_, ok := h.find("resolve recovered")
		return ok
	})

	r, _ := h.find("resolve recovered")
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", r.Level)
	}
	if got, _ := attrOf(r, logAttrField); got != "V" {
		t.Errorf("field = %q, want V", got)
	}
}

// TestLogsValidationRejected covers buildCandidate's validator.Validate
// failure branch.
func TestLogsValidationRejected(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lval")
	wp.set("level", "info", "l1")
	h, logger := newRecorder()

	type Config struct {
		Level string `source:"lval://level" validate:"oneof=debug info warn error"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("level", "BOGUS", "l2") // fails oneof validation
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "validation-rejected record", func() bool {
		_, ok := h.find("candidate rejected by validation; continuing to serve the previous config")
		return ok
	})

	r, _ := h.find("candidate rejected by validation; continuing to serve the previous config")
	if r.Level != slog.LevelError {
		t.Errorf("level = %v, want Error", r.Level)
	}
}

// TestLogsPreApplyRejected covers flush's PreApply-gate rejection branch.
func TestLogsPreApplyRejected(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lpre")
	wp.set("a", "first", "v1")
	h, logger := newRecorder()
	reject := errors.New("credential does not work")

	type Config struct {
		A string `source:"lpre://a"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
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

	waitUntil(t, 2*time.Second, "PreApply-rejected record", func() bool {
		_, ok := h.find("change rejected by PreApply; continuing to serve the previous config")
		return ok
	})

	r, _ := h.find("change rejected by PreApply; continuing to serve the previous config")
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", r.Level)
	}
	if got, _ := attrOf(r, logAttrCount); got != "1" {
		t.Errorf("count = %q, want 1", got)
	}
}

// TestLogsAppliedChange covers flush's successful-apply logging: one Info
// record per flush plus one Debug record per changed field.
func TestLogsAppliedChange(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("lapp")
	wp.set("a", "old", "v1")
	h, logger := newRecorder()

	type Config struct {
		A string `source:"lapp://a"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(wp), WithClock(clk), WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	wp.push("a", "new", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	waitUntil(t, 2*time.Second, "'config change applied' record", func() bool {
		_, ok := h.find("config change applied")
		return ok
	})

	r, _ := h.find("config change applied")
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", r.Level)
	}
	if got, _ := attrOf(r, logAttrCount); got != "1" {
		t.Errorf("count = %q, want 1", got)
	}

	fr, ok := h.find("field updated")
	if !ok {
		t.Fatalf("no 'field updated' record:\n%s", h.dump())
	}
	if fr.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", fr.Level)
	}
	if got, _ := attrOf(fr, logAttrField); got != "A" {
		t.Errorf("field = %q, want A", got)
	}
	if got, _ := attrOf(fr, logAttrVersion); got != "v2" {
		t.Errorf("version = %q, want v2", got)
	}
}

// TestLogsDroppedChangeEvent covers the failure this feature most exists for:
// a full dispatch queue silently discards the oldest event, and nothing else
// in mamori records that it happened.
func TestLogsDroppedChangeEvent(t *testing.T) {
	// Drive enqueue directly rather than racing a real watcher: the drop is a
	// property of the queue, and a queue of depth 1 overflowed twice is the
	// smallest thing that exercises it deterministically.
	h, logger := newRecorder()
	o := defaultOptions()
	WithLogger(logger)(o)
	WithQueueDepth(1)(o)

	e := &engine[struct{}]{o: o, dispatch: make(chan Change[struct{}], 1)}
	e.enqueue(Change[struct{}]{})
	e.enqueue(Change[struct{}]{}) // overflows, drops the first

	r, ok := h.find("change event dropped, dispatch queue full; the OnChange handler is not keeping up")
	if !ok {
		t.Fatalf("no dropped-event record:\n%s", h.dump())
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", r.Level)
	}
}

// TestLogsPollingFallback covers watchRef's non-watchable-provider branch: the
// log fires synchronously during Watch's own setup, before Watch returns.
func TestLogsPollingFallback(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	h, logger := newRecorder()
	p := &errAfterProvider{scheme: "lpoll", val: Value{Bytes: []byte("v"), Version: "v1"}}
	p.ok.Store(1000)

	type Config struct {
		V string `source:"lpoll://v"`
	}
	w, err := Watch[Config](context.Background(),
		WithProvider(p), WithClock(clk), WithLogger(logger),
		WithPollInterval(time.Minute),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	r, ok := h.find("provider has no native watch, polling")
	if !ok {
		t.Fatalf("no polling-fallback record:\n%s", h.dump())
	}
	if r.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", r.Level)
	}
	if got, _ := attrOf(r, logAttrScheme); got != "lpoll" {
		t.Errorf("scheme = %q, want lpoll", got)
	}
}
