# Meter Counters and the Prometheus Bridge

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `Meter` counters for the three failures the engine can now log but nobody can alert on, and add an `x/prom` bridge for shops that use `prometheus/client_golang` without OpenTelemetry.

**Architecture:** Three methods added to `mamori.Meter`, wired at the same call sites #70 added log lines to. `noopMeter` and `x/otel` are updated in the same change. `x/prom` is a new module implementing the full interface directly against `prometheus/client_golang`.

**Tech Stack:** Go 1.26, `prometheus/client_golang` for the new module, existing `go.opentelemetry.io/otel` for the updated one.

## Why the interface changes rather than gaining a sibling

Adding methods to `Meter` breaks any out-of-tree implementation. The alternative, an optional richer interface that mamori type-asserts, avoids that but leaves two interfaces and a branch at every call site forever.

The owner's position is that the project is new enough that a compile break for an implementor is acceptable and preferable to permanent API awkwardness. The only implementation in this repo, `x/otel`, is updated in the same change, so nothing here is left broken. The PR body must say plainly that this is a source-breaking change for anyone implementing `Meter`, and what the one-line fix is.

## Why `RejectReason` is a type, not a string

`RecordApplyRejected` needs to distinguish a validation rejection from a `PreApply` rejection. Passing a free `string` invites unbounded label cardinality in a Prometheus adapter, which is the classic way to melt a metrics backend. A typed constant with exactly two values makes the cardinality bound part of the API rather than a documentation request.

## Global Constraints

- Go 1.26+. Modules are tested with `GOWORK=off` from their own directory; core from the repo root.
- **Run `golangci-lint run` in every directory you touch.** CI gates on it with `default: standard`, which includes `unused`.
- Never use the em-dash character in any file, code comment, doc, or commit message.
- Core gains **no new dependency**. `x/prom` is its own module, so `prometheus/client_golang` never reaches a core consumer.
- **A metric label must never carry a value, a ref, or anything unbounded.** Labels are `scheme`, `status`, `error_kind`, and `reason`, all bounded sets. A ref as a label is both a cardinality bomb and a possible credential leak.
- `x/prom` must be registered in `.github/dependabot.yml` and `go.work`. The dependabot file is hand-maintained while CI discovers modules from disk, and a CI job cross-checks them: a missing entry fails the build. Verify with the command in Task 2 Step 6.
- Commits follow Conventional Commits. No `BREAKING CHANGE:` footer, per the owner's standing instruction, even though this is source-breaking for `Meter` implementors; call it out in the PR body instead.
- Do not run `git commit`; stage work and report it. The controller commits.

---

### Task 1: `Meter` counters

**Files:**
- Modify: `observ.go` (the interface and `noopMeter`)
- Modify: `reconciler.go` (three call sites)
- Modify: `x/otel/otel.go`, `x/otel/otel_test.go`, `x/otel/README.md`
- Create or modify: `observ_test.go` (a recording meter and the call-site tests)
- Modify: root `README.md`, the observability docs page (`grep -rln "WithMeter" site/`), and `skills/` (`grep -rln "WithMeter" skills/`)

**Interfaces:**
- Produces: `Meter` gains `RecordStale(scheme string)`, `RecordChangeDropped()`, `RecordApplyRejected(reason RejectReason)`; the `RejectReason` type with `RejectValidation` and `RejectPreApply`.

Read `observ.go` first, then #70's log call sites in `reconciler.go`: the three new
counters go at exactly the same places, so `grep -n "o.log()\." reconciler.go` finds
them.

- [ ] **Step 1: Write the failing tests**

Add to `observ_test.go` (create it if absent). A recording meter is needed for
every case, so build it first:

```go
// recordingMeter counts every Meter call, so a test can assert an engine event
// reached the metrics sink without needing a real backend.
type recordingMeter struct {
	mu             sync.Mutex
	resolves       int
	refreshes      int
	watchErrors    int
	stale          []string
	dropped        int
	applyRejected  []RejectReason
}

func (m *recordingMeter) RecordResolve(string, time.Duration, error) {
	m.mu.Lock(); defer m.mu.Unlock(); m.resolves++
}
func (m *recordingMeter) RecordRefresh(string) {
	m.mu.Lock(); defer m.mu.Unlock(); m.refreshes++
}
func (m *recordingMeter) RecordWatchError(string) {
	m.mu.Lock(); defer m.mu.Unlock(); m.watchErrors++
}
func (m *recordingMeter) RecordStale(scheme string) {
	m.mu.Lock(); defer m.mu.Unlock(); m.stale = append(m.stale, scheme)
}
func (m *recordingMeter) RecordChangeDropped() {
	m.mu.Lock(); defer m.mu.Unlock(); m.dropped++
}
func (m *recordingMeter) RecordApplyRejected(r RejectReason) {
	m.mu.Lock(); defer m.mu.Unlock(); m.applyRejected = append(m.applyRejected, r)
}

func (m *recordingMeter) droppedCount() int {
	m.mu.Lock(); defer m.mu.Unlock(); return m.dropped
}
func (m *recordingMeter) rejections() []RejectReason {
	m.mu.Lock(); defer m.mu.Unlock(); return append([]RejectReason(nil), m.applyRejected...)
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
```

Construct the `engine[T]` literal the way `logging_test.go`'s equivalent test
does rather than inventing a new path; that test already solved this.

Also write, in the same shape:
- `TestMeterCountsValidationRejection`, asserting one `RejectValidation`.
- `TestMeterCountsPreApplyRejection`, asserting one `RejectPreApply`.
- `TestMeterCountsStale`, asserting the scheme is recorded.

Model these on the corresponding tests in `logging_test.go`, which already
drive each of these paths; reuse their setup rather than rebuilding it.

- [ ] **Step 2: Run and confirm failure**

```bash
go test -run TestMeterCounts ./...
```

Expected: FAIL, undefined methods.

- [ ] **Step 3: Extend the interface**

In `observ.go`:

```go
// RejectReason names why a candidate configuration was refused. It is a closed
// set of two so an adapter can use it as a metric label without unbounded
// cardinality, which a free-form string would invite.
type RejectReason string

const (
	// RejectValidation means the candidate failed the configured Validator.
	RejectValidation RejectReason = "validation"
	// RejectPreApply means a PreApply hook refused the change.
	RejectPreApply RejectReason = "preapply"
)
```

Add to the `Meter` interface, with doc comments matching the existing three:

```go
	// RecordStale reports that a value has not refreshed within the WithStale
	// threshold.
	RecordStale(scheme string)
	// RecordChangeDropped reports that a change event was discarded because the
	// OnChange dispatch queue was full. A non-zero rate means handlers are not
	// keeping up and callers are missing changes.
	RecordChangeDropped()
	// RecordApplyRejected reports that a candidate configuration was refused
	// and the previous one is still being served.
	RecordApplyRejected(reason RejectReason)
```

Extend `noopMeter` with the three no-op implementations.

- [ ] **Step 4: Wire the call sites**

Exactly where #70 put its log lines, so `grep -n "o.log()\." reconciler.go`
locates all three:

- beside the "value is stale" log: `e.o.meter.RecordStale(ref.Scheme)`
- beside the "candidate rejected by validation" log: `e.o.meter.RecordApplyRejected(RejectValidation)`
- beside the "change rejected by PreApply" log: `e.o.meter.RecordApplyRejected(RejectPreApply)`
- inside `enqueue`'s drop branch, beside its log: `e.o.meter.RecordChangeDropped()`

Add nothing else. No control flow changes.

- [ ] **Step 5: Update `x/otel`**

Add three instruments following the existing `NewMeter` pattern exactly: a
counter each for stale values, dropped change events, and rejected applies.
Name them consistently with the existing `MetricResolveDuration`,
`MetricRefreshCount`, `MetricWatchErrors` constants, and add the new names as
exported constants beside them.

The rejected-applies counter takes a `reason` attribute carrying the
`RejectReason`. The stale counter takes `scheme`. The dropped counter takes no
attributes: it is a process-wide property of the dispatch queue, not a
per-scheme one.

Update `x/otel/otel_test.go` to cover the three new methods, matching how the
existing three are tested, and `x/otel/README.md`'s metric table.

- [ ] **Step 6: Verify**

```bash
go test ./... && go test -race ./... && go vet ./... && golangci-lint run
cd x/otel && GOWORK=off go test ./... && GOWORK=off go vet ./... && golangci-lint run
```

Then confirm nothing else broke:

```bash
for d in $(find . -name go.mod -not -path "*/node_modules/*" | xargs -n1 dirname | sort); do
  (cd "$d" && GOWORK=off go test ./... >/dev/null 2>&1) || echo "FAIL $d"
done
```

Expected: no output from the loop.

- [ ] **Step 7: Verify each counter has teeth**

For each of the four call sites, delete the `RecordX` line, confirm the
matching test fails, restore it exactly. Report each failure message.

- [ ] **Step 8: Docs**

Root `README.md` and the observability page: document the three new methods and
the `RejectReason` values, and state plainly that implementing `Meter` now
requires them. `skills/`: one line. Note in the docs that `RecordChangeDropped`
is the signal that `OnChange` handlers are too slow.

- [ ] **Step 9: Stage**

```bash
go test ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.

---

### Task 2: the `x/prom` module

**Files:**
- Create: `x/prom/go.mod`, `x/prom/go.sum`, `x/prom/prom.go`, `x/prom/prom_test.go`, `x/prom/README.md`
- Create: `site/src/pages/docs/prometheus.md`
- Modify: `go.work`, `.github/dependabot.yml`, `site/src/layouts/DocsLayout.astro`, and the docs index that lists the otel page

**Interfaces:**
- Consumes: `mamori.Meter`, `mamori.RejectReason`, `mamori.ErrorKind` from Task 1.
- Produces: `New(reg prometheus.Registerer) (mamori.Meter, error)`.

Read `x/otel/otel.go` and `x/otel/README.md` first. This module is the same
shape with a different backend, and should read like a sibling of it, not like
a new invention.

- [ ] **Step 1: Scaffold**

```bash
mkdir -p x/prom
cd x/prom
cat > go.mod <<'EOF'
module github.com/xavidop/mamori/x/prom

go 1.26

require (
	github.com/prometheus/client_golang v1.24.0
	github.com/xavidop/mamori v0.0.0
)

replace github.com/xavidop/mamori => ../..
EOF
GOWORK=off go get github.com/prometheus/client_golang@latest
GOWORK=off go mod tidy
```

Use whatever version `go get` resolves; pin it in `go.mod` as it lands. Add
`./x/prom` to `go.work`.

- [ ] **Step 2: Write the failing tests**

Prometheus ships `testutil` for exactly this, so assert on rendered metric
output rather than on internal state:

```go
package prom

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
// dies from unbounded label values, so the only two reasons that can ever
// appear are the two mamori defines.
func TestRejectReasonIsABoundedLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := New(reg)
	m.RecordApplyRejected(mamori.RejectValidation)
	m.RecordApplyRejected(mamori.RejectPreApply)

	out, err := testutil.CollectAndFormat(reg, "text")
	if err != nil {
		t.Fatalf("CollectAndFormat: %v", err)
	}
	s := string(out)
	for _, want := range []string{`reason="validation"`, `reason="preapply"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in:\n%s", want, s)
		}
	}
}

// TestNoRefOrValueEverBecomesALabel guards the property that matters most: a
// label carrying a ref would be both a cardinality bomb and a credential leak.
func TestNoRefOrValueEverBecomesALabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := New(reg)
	m.RecordResolve("aws-sm", time.Millisecond, errors.New("boom"))
	m.RecordStale("aws-sm")
	m.RecordChangeDropped()

	out, _ := testutil.CollectAndFormat(reg, "text")
	for _, forbidden := range []string{"ref=", "value=", "://"} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("metric output contains %q:\n%s", forbidden, out)
		}
	}
}
```

Confirm `testutil.CollectAndFormat`'s real signature against the resolved
version before relying on it; if it differs, use `testutil.GatherAndCompare` or
gather and render manually, and say so in your report.

- [ ] **Step 3: Implement**

`New(reg prometheus.Registerer) (mamori.Meter, error)` builds and registers:

- a histogram for resolve duration, labels `scheme` and `status`, plus an
  `error_kind` label carrying `mamori.ErrorKind` on failure
- counters for refreshes and watch errors, label `scheme`
- a counter for stale values, label `scheme`
- a counter for dropped change events, no labels
- a counter for rejected applies, label `reason`

Return the registration error rather than panicking, matching `x/otel`'s
`NewMeter`, so a caller registering twice gets an error instead of a crash.

Metric names use a `mamori_` prefix and follow Prometheus convention
(`_seconds` for a duration histogram, `_total` for counters). Note that
`x/otel` records milliseconds; Prometheus convention is seconds, so convert.
Say so in the README, since the same event will read differently on the two
backends.

- [ ] **Step 4: Verify**

```bash
cd x/prom
GOWORK=off go test ./... && GOWORK=off go test -race ./... && GOWORK=off go vet ./... && golangci-lint run
```

- [ ] **Step 5: Verify the guards have teeth**

Add a `ref` label to the resolve histogram and confirm
`TestNoRefOrValueEverBecomesALabel` fails. Restore. Report the message.

- [ ] **Step 6: Register the module with CI**

Add the `x/prom` entry to `.github/dependabot.yml` beside the other `x/`
entries, then verify:

```bash
cd <repo root>
dirs=$(find . -name go.mod -not -path './site/*' -not -path './**/testdata/*' -exec dirname {} \; | sort | jq -R -s -c 'split("\n") | map(select(length > 0))')
python3 .github/scripts/check_dependabot_coverage.py "$dirs"
```

Expected: "All N Go modules are covered by dependabot."

- [ ] **Step 7: Docs**

`x/prom/README.md` with the metric table, install, and a usage example;
`site/src/pages/docs/prometheus.md` mirroring it and matching the otel page's
structure; a sidebar entry; and a line wherever the otel bridge is listed so
the two are discoverable together. State that a shop already using OpenTelemetry
should use `x/otel` with an OTLP or Prometheus exporter rather than this, and
that this exists for shops using `client_golang` directly.

- [ ] **Step 8: Stage**

```bash
cd x/prom && GOWORK=off go test ./... && cd ../.. && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.
