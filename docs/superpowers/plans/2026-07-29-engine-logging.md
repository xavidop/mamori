# Engine Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the mamori engine structured logging through `log/slog`, so an operator can see resolve failures, rejected candidates, applied changes, staleness, and dropped events, none of which are visible today.

**Architecture:** A new `WithLogger` option storing a `*slog.Logger` on `options`, defaulting to `slog.DiscardHandler` so the library stays silent unless asked. A small helper file defines the shared attribute vocabulary; call sites across `resolve.go` and `reconciler.go` are one line each. Values are never logged, and refs go through the existing `redactRef`.

**Tech Stack:** Go 1.26, `log/slog` (stdlib, no new dependency).

**Spec:** `docs/superpowers/specs/2026-07-29-engine-logging-design.md`

## Global Constraints

- Go 1.26+. Core is tested from the repo root with `go test ./...`.
- **Run `golangci-lint run` at the repo root before reporting done.** CI gates on it with `default: standard`, which includes `unused`.
- Never use the em-dash character in any file, code comment, doc, or commit message.
- **No new dependency.** `log/slog` is stdlib. Core's dependencies are deliberately minimal and this must not add to them.
- **A resolved value must never reach a log record.** Not `Value.Bytes`, not a decoded field, not "the value that failed validation". Refs go through `redactRef`. This is tested, not just intended.
- This change does **not** modify the `Meter` or `Tracer` interfaces. Adding methods there breaks every implementor including `x/otel`, and that decision is out of scope here.
- Commits follow Conventional Commits, scope `feat(logging):` or `feat:`. No `BREAKING CHANGE:` footer.
- Do not run `git commit`; stage work and report it. The controller commits.

---

### Task 1: `WithLogger` and the engine's log sites

**Files:**
- Create: `logging.go`
- Create: `logging_test.go`
- Modify: `reconcile.go` (the `options` struct and `defaultOptions`)
- Modify: `resolve.go` (initial-resolve failure)
- Modify: `reconciler.go` (watch error, stale, validation, PreApply, applied, recovery, dropped event, polling fallback)
- Modify: `README.md` (options list)
- Modify: `site/src/pages/docs/observability.md` if it exists; otherwise the docs page that documents `WithMeter`/`WithTracer`. Find it with `grep -rln "WithMeter" site/`.
- Modify: `skills/mamori/SKILL.md` and/or `skills/mamori/references/` where options are listed. Find with `grep -rln "WithMeter\|OnError" skills/`.

**Interfaces:**
- Produces: `WithLogger(l *slog.Logger) Option`; `(o *options) log() *slog.Logger`; the unexported attribute-key constants and event helpers in `logging.go`.
- Consumes: `redactRef(Ref) string` from `status.go`, `ErrorKind(error) Kind` from `errors.go`.

Read `observ.go` first: it holds `Meter`, `Tracer`, and their no-op defaults, and its doc comments explain the no-dependency stance this option must match.

- [ ] **Step 1: Write the failing tests**

Create `logging_test.go`. The recording handler is the backbone of every test
here, so build it first.

```go
package mamori

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
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
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test -run 'TestLogger|TestWithLogger' ./...
```

Expected: FAIL, `o.logger undefined`.

- [ ] **Step 3: Add the option**

In `reconcile.go`, add to the `options` struct beside `meter` and `tracer`:

```go
	logger *slog.Logger
```

In `defaultOptions()`, beside `meter: noopMeter{}`:

```go
		logger: slog.New(slog.DiscardHandler),
```

Add `"log/slog"` to that file's imports.

Then create `logging.go`:

```go
package mamori

import (
	"log/slog"
)

// WithLogger installs a structured logger for engine events: resolve failures
// and recoveries, watch errors, rejected candidates, applied changes, stale
// values, and dropped change events.
//
// mamori logs nothing by default. A library that writes to the application's
// stderr merely because it was linked in has taken a decision that belongs to
// the application, so the zero configuration is a discard logger and
// WithLogger(slog.Default()) is the one-line opt-in. Passing nil resets to
// silent rather than panicking, so a caller wiring this up conditionally can
// pass nil for the off case.
//
// Two things worth knowing before choosing a handler.
//
// Records never contain a resolved value. They carry the field path, the
// scheme, the ref with inline credentials redacted, the version, and the
// error. That is deliberate and tested: a config log is exactly the artifact
// most likely to be shipped off the host, so a secret must never reach it.
//
// The handler is called from the reconciler goroutine, so a handler that
// blocks blocks reconciliation, the same constraint OnError carries. A handler
// writing synchronously to a remote collector will stall the engine; buffer it.
//
// WithLogger and OnError are independent and may both be set. An error reaches
// both, and neither suppresses the other.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l == nil {
			l = slog.New(slog.DiscardHandler)
		}
		o.logger = l
	}
}

// log returns the configured logger, never nil.
//
// Call sites go through this rather than touching o.logger directly, so a
// future change to the default (or to how a nil is handled) has one place to
// live.
func (o *options) log() *slog.Logger {
	if o.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.logger
}

// Attribute keys, fixed and shared across every call site. A consistent
// vocabulary is what makes structured output queryable, and is the whole
// reason to emit records rather than formatted strings, so these are constants
// rather than string literals repeated at each site.
const (
	logAttrField   = "field"   // dotted struct path, e.g. "Redis.Password"
	logAttrScheme  = "scheme"  // provider scheme, e.g. "aws-sm"
	logAttrRef     = "ref"     // the ref, with sensitive query options redacted
	logAttrVersion = "version" // provider version of the value
	logAttrKind    = "kind"    // the mamori.Kind classification of an error
	logAttrErr     = "err"     // the error text
	logAttrCount   = "count"   // how many items an event covers
)

// errAttrs renders an error as its text plus its classification, so an
// operator can filter on kind without parsing messages.
func errAttrs(err error) []any {
	if err == nil {
		return nil
	}
	return []any{logAttrErr, err.Error(), logAttrKind, string(ErrorKind(err))}
}
```

- [ ] **Step 4: Run the option tests**

```bash
go test -run 'TestLogger|TestWithLogger' ./...
```

Expected: PASS.

- [ ] **Step 5: Add the call sites**

Each is one line. Add them at these exact places; read the surrounding code
first so the available variables match.

**`resolve.go`, in `resolveRef`**, on the provider-error return after
`o.meter.RecordResolve(...)`:

```go
	if err != nil {
		o.log().Warn("resolve failed",
			append([]any{logAttrScheme, ref.Scheme, logAttrRef, redactRef(ref)}, errAttrs(err)...)...)
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	}
```

**`reconciler.go` around line 979**, the watch-error path, beside
`e.o.meter.RecordWatchError(ref.Scheme)`:

```go
	e.o.log().Warn("watch error",
		append([]any{logAttrScheme, ref.Scheme, logAttrRef, redactRef(ref)}, errAttrs(err)...)...)
```

**`reconciler.go` around line 983**, where `StaleError` is built:

```go
	e.o.log().Warn("value is stale",
		logAttrField, spec.Path, logAttrRef, redactRef(ref),
		logAttrErr, se.Error())
```

**`reconciler.go` around lines 663 and 965**, where `delete(e.lastErr, spec.Path)`
clears a previous failure. Log a recovery only when there was an error to
recover from, so a healthy refresh stays quiet:

```go
	if _, had := e.lastErr[spec.Path]; had {
		e.o.log().Info("resolve recovered", logAttrField, spec.Path)
	}
	delete(e.lastErr, spec.Path)
```

**`reconciler.go` around line 1041**, where validation rejects a candidate:

```go
	e.o.log().Error("candidate rejected by validation; continuing to serve the previous config",
		errAttrs(err)...)
```

**`reconciler.go` around line 1163**, where `runPreApply` returns an error:

```go
	e.o.log().Warn("change rejected by PreApply; continuing to serve the previous config",
		append([]any{logAttrCount, len(fields)}, errAttrs(err)...)...)
```

**`reconciler.go`, in `flush`** where a change is successfully applied and the
snapshot swaps. Log once per flush with the count, and one Debug record per
field so the detail is available without making the Info line unbounded:

```go
	e.o.log().Info("config change applied", logAttrCount, len(fields))
	for _, f := range fields {
		e.o.log().Debug("field updated",
			logAttrField, f.Path, logAttrVersion, f.NewVersion)
	}
```

Check the real field-struct member names before writing this; the plan's
`f.Path` and `f.NewVersion` follow `Change[T]`'s `Fields`, so confirm against
the actual type and use its real names.

**`reconciler.go`, in `enqueue` at the drop branch** (currently line 1540):

```go
		default:
			select {
			case <-e.dispatch: // drop oldest, retry
				e.o.log().Warn("change event dropped, dispatch queue full; the OnChange handler is not keeping up",
					logAttrCount, cap(e.dispatch))
			default:
			}
```

**The polling fallback**, wherever the engine chooses `pollWatch` because a
provider does not implement `WatchableProvider`. Find it with
`grep -n "pollWatch(" reconciler.go`:

```go
	e.o.log().Debug("provider has no native watch, polling",
		logAttrScheme, ref.Scheme, logAttrRef, redactRef(ref))
```

- [ ] **Step 6: Write the secret-safety tests**

These are the reason this feature is safe. Add to `logging_test.go`:

```go
// TestLoggerNeverLogsAValue is the load-bearing safety property. A config log
// is precisely the artifact most likely to be shipped off the host, so a
// resolved value must never reach one. This drives a real Load with a provider
// returning a known sentinel and scans everything written at Debug level.
func TestLoggerNeverLogsAValue(t *testing.T) {
	const sentinel = "s3cr3t-sentinel-value-do-not-log"

	h, logger := newRecorder()
	type Config struct {
		Password string `source:"test://pw"`
	}
	_, err := Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(staticProvider{scheme: "test", value: sentinel}),
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
		V string `source:"test://thing?token=hunter2"`
	}
	// A failing provider guarantees the ref reaches a log line.
	_, _ = Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(staticProvider{scheme: "test", err: errors.New("boom")}),
	)
	got := h.dump()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("an inline credential reached the log:\n%s", got)
	}
	if !strings.Contains(got, "token=") {
		t.Fatalf("expected the redacted ref to still name the option:\n%s", got)
	}
}
```

`staticProvider` is a stand-in. Check `mamoritest/` and the existing core test
files for an in-repo test provider and use that instead of writing a new one;
`grep -rn "type .*[Pp]rovider struct" *_test.go mamoritest/` will find it. Only
write one if none exists, and if you do, keep it minimal.

Add `"errors"` to the test imports.

- [ ] **Step 7: Write the event tests**

One per call site. They follow the same shape; here are two, write the rest to
match:

```go
func TestLogsResolveFailure(t *testing.T) {
	h, logger := newRecorder()
	type Config struct {
		V string `source:"test://thing"`
	}
	_, _ = Load[Config](context.Background(),
		WithLogger(logger),
		WithProvider(staticProvider{scheme: "test", err: errors.New("boom")}),
	)
	r, ok := h.find("resolve failed")
	if !ok {
		t.Fatalf("no 'resolve failed' record:\n%s", h.dump())
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn: the field still serves its last good value, so this is not an outage", r.Level)
	}
	if got, _ := attrOf(r, logAttrScheme); got != "test" {
		t.Errorf("scheme = %q, want %q", got, "test")
	}
	if got, _ := attrOf(r, logAttrKind); got == "" {
		t.Error("no kind attribute; an operator cannot filter by classification")
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
```

The `engine[T]` literal in the second test may need more fields to be usable;
construct it however the existing reconciler tests do (`grep -n "engine\[" *_test.go`)
rather than inventing a new construction path.

Write the remaining tests for: watch error, stale, validation rejection,
PreApply rejection, applied change, and resolve recovery. For the timing
sensitive ones use the existing `FakeClock`, as the reconciler tests do; do not
add sleeps.

- [ ] **Step 8: Verify each log site has teeth**

For each of the eight events, delete its log call, confirm the corresponding
test fails, and restore it exactly. Report the failure message for each. A log
line nothing asserts on is a line that will silently disappear in a later
refactor.

- [ ] **Step 9: Full verification**

```bash
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
go build ./...
```

Then confirm nothing else in the monorepo broke:

```bash
for d in $(find . -name go.mod -not -path "*/node_modules/*" | xargs -n1 dirname | sort); do
  (cd "$d" && GOWORK=off go test ./... >/dev/null 2>&1) || echo "FAIL $d"
done
```

Expected: no output from the loop.

- [ ] **Step 10: Docs**

1. Root `README.md`: add `WithLogger` where the options are listed, one line, noting it is silent by default.
2. The observability docs page (find it with `grep -rln "WithMeter" site/`): add a section covering `WithLogger`, the silent default, the full table of logged events with their levels, the promise that values are never logged and refs are redacted, and the blocking-handler constraint. Match the page's existing structure.
3. `skills/` (find with `grep -rln "WithMeter\|OnError" skills/`): add `WithLogger` alongside the other options, one line.
4. Confirm the `WithLogger` doc comment in `logging.go` covers all three of: silent default, blocking handler, values never logged.

- [ ] **Step 11: Stage**

```bash
go test ./... && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.
