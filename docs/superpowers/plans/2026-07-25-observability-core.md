# Workstream B (core): Status, Health, and Doctor

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a running `Watcher` introspectable and make config resolvability checkable before deploy, by adding a per-field `Report`, `Watcher.Status()`/`Health()`, and a one-shot `Doctor[T]`.

**Architecture:** The reconciler goroutine already owns per-field maps (`observed`, `applied`, `lastOK`) with no locks. Rather than add a mutex on the hot path, the engine builds an immutable `*Report` and publishes it through an `atomic.Pointer[Report]`, mirroring how `Get()` already publishes the config. `Status()` loads that pointer and recomputes age and staleness against the clock at read time. `Doctor[T]` reuses `fieldSpecs` and a new non-fail-fast resolve to probe every field once without starting a watcher. This plan introduces the monotonic snapshot version counter that later history/pin work (workstream D) builds on.

**Tech Stack:** Go 1.26, stdlib only. No new dependencies in core.

This is the core slice of spec section 7 (`docs/superpowers/specs/2026-07-24-operational-layer-design.md`), the HTTP handler and authenticator (§7.5, §7.6) are a separate later plan. Error classification (`Kind`, `ErrorKind`) from workstream A is complete and consumed here.

## Global Constraints

- **Core dependencies are frozen.** `github.com/xavidop/mamori` may import only stdlib, `validator/v10`, `mapstructure/v2`, `fsnotify`, `yaml.v3`, and `goleak` (test-only). This plan adds nothing.
- **Do not run `git commit`.** Stage with `git add` and report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command.** `make test` runs from the repo root.
- **The tree stays green after every task.** All 34 modules must pass at every stopping point. This plan touches only core, but `make test` still runs repo-wide because provider modules depend on core via `replace`.
- **No em-dash characters** anywhere.
- **Concurrency is not optional here.** `Status()` runs on a caller goroutine while the reconciler goroutine mutates engine state. Every field the reconciler writes and `Status` reads must go through the `atomic.Pointer[Report]`, never a direct map read. All new tests run under `-race`, and the goroutine-hygiene invariant (`goleak` passes on `Close`) must hold.
- Doc comments on every exported symbol, explaining the why, matching the existing voice in `reconciler.go` and `errors.go`.

---

### Task 1: Report types and ref redaction

**Files:**
- Create: `status.go`
- Create: `status_test.go`

**Interfaces:**
- Consumes: `mamori.Kind` from workstream A; `secret.Redacted` from `secret/`.
- Produces: `FieldStatus`, `Report`, and an unexported `redactRef(ref Ref) string`. Tasks 2 and 3 populate and return these.

**Design note.** `Report` and `FieldStatus` carry only the fields this workstream populates. Later workstreams add fields to them: `Pinned bool` and a history version in D, `Candidates []string` for source chains in E. Adding a struct field is non-breaking, so they are omitted here rather than stubbed.

- [ ] **Step 1: Write the failing test**

Create `status_test.go`:

```go
package mamori

import (
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/mamori/secret"
)

func TestRedactRefLeavesPlainRefUntouched(t *testing.T) {
	ref, err := ParseRef("aws-sm://prod/db#password")
	if err != nil {
		t.Fatal(err)
	}
	got := redactRef(ref)
	if !strings.Contains(got, "prod/db") {
		t.Fatalf("redactRef dropped the path: %q", got)
	}
	if strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef redacted a ref with no sensitive opts: %q", got)
	}
}

func TestRedactRefHidesSensitiveOpts(t *testing.T) {
	// A ref that carries an inline credential as a query option must not leak it
	// through a report that is designed to be served over HTTP.
	ref := Ref{
		Scheme: "vault",
		Path:   "kv/data/api",
		Opts:   url.Values{"token": {"s.hunter2"}, "namespace": {"team-a"}},
		Raw:    "vault://kv/data/api?token=s.hunter2&namespace=team-a",
	}
	got := redactRef(ref)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("redactRef leaked the token value: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef did not redact the token opt: %q", got)
	}
	if !strings.Contains(got, "team-a") {
		t.Fatalf("redactRef wrongly hid a non-sensitive opt: %q", got)
	}
	if !strings.Contains(got, "kv/data/api") {
		t.Fatalf("redactRef dropped the path: %q", got)
	}
}

func TestRedactRefDenylistIsCaseInsensitive(t *testing.T) {
	ref := Ref{
		Scheme: "x",
		Path:   "p",
		Opts:   url.Values{"APIKey": {"abc"}, "Password": {"pw"}, "SAS": {"sig"}},
		Raw:    "x://p?APIKey=abc&Password=pw&SAS=sig",
	}
	got := redactRef(ref)
	for _, leak := range []string{"abc", "pw", "sig"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redactRef leaked a value under a mixed-case sensitive key: %q", got)
		}
	}
}
```

- [ ] **Step 2: Run it, confirm it fails**

```bash
GOWORK=off go test ./... -run TestRedactRef -v
```

Expected: compile failure, `undefined: redactRef`, `undefined: FieldStatus` is not referenced yet so only `redactRef` fails.

- [ ] **Step 3: Implement `status.go`**

```go
package mamori

import (
	"net/url"
	"strings"
	"time"

	"github.com/xavidop/mamori/secret"
)

// FieldStatus is the live state of one configured field, as reported by
// Watcher.Status and Doctor. It is safe to serialize and safe to serve over
// HTTP: Ref has sensitive query options redacted, and no field value appears.
type FieldStatus struct {
	Path      string        // dotted field path, e.g. "Redis.Password"
	Scheme    string        // provider scheme of the ref
	Ref       string        // the ref, with sensitive query options redacted
	Version   string        // provider version of the currently observed value
	LastOK    time.Time     // last successful resolve, zero if never
	Age       time.Duration // GeneratedAt minus LastOK, recomputed at read time
	Stale     bool          // Age exceeds the configured WithStale threshold
	LastError string        // text of the last resolve error, empty if none
	LastKind  Kind          // classification of LastError, empty if none
	Sensitive bool          // field is a secret.String or secret.Bytes
}

// Report is a point-in-time snapshot of a Watcher's health, or the result of a
// one-shot Doctor probe. Fields are in struct declaration order.
type Report struct {
	Fields      []FieldStatus
	Snapshot    uint64    // version of the snapshot Get currently returns
	Live        uint64    // newest validated snapshot; equals Snapshot until pinning lands
	Healthy     bool      // no field is stale or carries a terminal error kind
	GeneratedAt time.Time // when this report was built
}

// sensitiveOptKeys are query-option names whose values are redacted from a Ref
// before it appears in a Report. Refs are not generally secret, but some
// providers accept an inline credential as an option, and a Report is designed
// to be safe to serve over HTTP.
var sensitiveOptKeys = map[string]struct{}{
	"token": {}, "password": {}, "secret": {}, "key": {},
	"apikey": {}, "api_key": {}, "sas": {}, "credential": {},
}

// redactRef renders ref as a string with any sensitive query-option value
// replaced by secret.Redacted. The scheme, path, key, and non-sensitive options
// are preserved so the ref stays useful for diagnostics.
func redactRef(ref Ref) string {
	if len(ref.Opts) == 0 {
		if ref.Raw != "" {
			return ref.Raw
		}
		return ref.String()
	}
	safe := make(url.Values, len(ref.Opts))
	for name, vals := range ref.Opts {
		if _, sensitive := sensitiveOptKeys[strings.ToLower(name)]; sensitive {
			safe[name] = []string{secret.Redacted}
			continue
		}
		safe[name] = vals
	}
	redacted := Ref{Scheme: ref.Scheme, Path: ref.Path, Key: ref.Key, Opts: safe}
	return redacted.String()
}
```

Verify `secret.Redacted` is exported (it is used by `secret.String.String()`); if it is unexported, use the exported constant the secret package provides and adjust the test import accordingly, reporting the change.

- [ ] **Step 4: Run it, confirm pass**

```bash
GOWORK=off go test ./... -run TestRedactRef -v
GOWORK=off go vet ./...
```

- [ ] **Step 5: Stage**

```bash
git add status.go status_test.go
```

```
feat(core): add Report and FieldStatus types with ref redaction

Introduces the report types that Status and Doctor return, plus redactRef,
which hides inline credentials carried as query options so a report is safe
to serve over HTTP. No behavior change yet; the types are populated in the
following commits.
```

---

### Task 2: Snapshot version counter and Report publishing in the engine

**Files:**
- Modify: `reconciler.go` (the `engine[T]` struct, `Watch`, `start`, `loop`, `flush`, `handleErr`; add report-building)
- Modify: `status_test.go` (add engine-level tests)

**Interfaces:**
- Consumes: `FieldStatus`, `Report`, `redactRef` from Task 1; `ErrorKind` from workstream A.
- Produces: `Watcher.Status() Report` and `Watcher.Health() error`, plus a `HealthError` type. Task 3 (`Doctor`) reuses `Report` and the report-building helper.

**Design.** Add three things to the engine:
1. A `version uint64`, starting at 1 for the initial snapshot, incremented on each `flush` that applies a non-empty diff.
2. A `lastErr map[string]error` recording the most recent error per path (set in `handleErr`, cleared on a successful observe in `loop`).
3. An `atomic.Pointer[Report]` on the `Watcher`, rebuilt and stored at the end of each `loop` iteration and at the end of `flush`.

`Status()` loads the pointer and recomputes `Age`/`Stale` against `clock.Now()` so an idle engine does not report stale ages as fresh. Build the report from engine-owned state only inside the reconciler goroutine; never read the engine maps from `Status()`.

- [ ] **Step 1: Write the failing tests**

Add to `status_test.go`. Use the existing test provider and `FakeClock` patterns from the repo (read `watch_test.go` and `clock_test.go` for how existing watch tests drive the engine; reuse their in-repo test provider rather than inventing one).

```go
func TestStatusReportsResolvedFields(t *testing.T) {
	// Build a Watcher over a struct with two env-backed fields, then assert
	// Status returns a healthy report naming both fields at version 1.
	t.Setenv("MAMORI_STATUS_A", "alpha")
	t.Setenv("MAMORI_STATUS_B", "beta")

	type Config struct {
		A string `source:"env:MAMORI_STATUS_A"`
		B string `source:"env:MAMORI_STATUS_B"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	rep := w.Status()
	if rep.Snapshot != 1 {
		t.Fatalf("initial Snapshot = %d, want 1", rep.Snapshot)
	}
	if rep.Live != rep.Snapshot {
		t.Fatalf("Live %d != Snapshot %d with no pinning", rep.Live, rep.Snapshot)
	}
	if !rep.Healthy {
		t.Fatalf("fresh config reported unhealthy: %+v", rep)
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Status reported %d fields, want 2", len(rep.Fields))
	}
	for _, f := range rep.Fields {
		if f.LastKind != "" || f.LastError != "" {
			t.Errorf("field %s carries an error on a clean load: %q %q", f.Path, f.LastKind, f.LastError)
		}
		if f.LastOK.IsZero() {
			t.Errorf("field %s has zero LastOK after a successful load", f.Path)
		}
	}
}

func TestHealthNilWhenAllFresh(t *testing.T) {
	t.Setenv("MAMORI_HEALTH_A", "x")
	type Config struct {
		A string `source:"env:MAMORI_HEALTH_A"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Health(); err != nil {
		t.Fatalf("Health on fresh config = %v, want nil", err)
	}
}

func TestStatusConcurrentWithReconcile(t *testing.T) {
	// Run Status in a tight loop on one goroutine while the engine reconciles on
	// another. This must be clean under -race. It asserts nothing about values,
	// only that concurrent Status is safe.
	t.Setenv("MAMORI_RACE_A", "v0")
	type Config struct {
		A string `source:"env:MAMORI_RACE_A"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = w.Status()
			_ = w.Health()
		}
		close(done)
	}()
	<-done
}
```

The staleness and error-classification behavior are driven with a controllable provider and `FakeClock`; add a test using the repo's in-memory test provider (or the one workstream C will provide, if it exists yet) that resolves once, then fails, and asserts `Status().Fields[i].LastKind` reflects the injected kind and `Stale` flips after the clock advances past `WithStale`. If no controllable in-repo provider exists yet, write this specific test against a small local provider defined in `status_test.go`, and note that it duplicates a fixture the mamoritest package will later generalize.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run 'TestStatus|TestHealth' -v
```

Expected: `w.Status undefined`, `w.Health undefined`.

- [ ] **Step 3: Add version, lastErr, and the report pointer**

In `reconciler.go`, add to `Watcher[T]`:

```go
	report atomic.Pointer[Report]
```

Add to `engine[T]`:

```go
	version uint64            // monotonic snapshot version, starts at 1
	lastErr map[string]error  // most recent error per path, reconciler-owned
```

In `Watch`, after building the engine and seeding `observed`/`applied`/`lastOK`, initialize `e.version = 1` and `e.lastErr = make(map[string]error, len(specs))`, then publish the initial report before `e.start(wctx)`:

```go
	e.version = 1
	w.report.Store(e.buildReport())
```

- [ ] **Step 4: Implement `buildReport`, `Status`, `Health`**

Add to `reconciler.go` (or a new `report.go` if `reconciler.go` grows unwieldy; match the repo's file-size norms):

```go
// buildReport constructs an immutable Report from the engine's current state. It
// is called only by the reconciler goroutine, so it reads the engine maps
// without locking. Age and Stale are left as of build time and recomputed by
// Status at read time.
func (e *engine[T]) buildReport() *Report {
	now := e.o.clock.Now()
	fields := make([]FieldStatus, 0, len(e.specs))
	healthy := true
	for _, spec := range e.specs {
		fs := FieldStatus{
			Path:      spec.Path,
			Scheme:    spec.Ref.Scheme,
			Ref:       redactRef(spec.Ref),
			Sensitive: spec.Sensitive,
		}
		if v, ok := e.observed[spec.Path]; ok {
			fs.Version = v.Version
		}
		if last, ok := e.lastOK[spec.Path]; ok {
			fs.LastOK = last
			fs.Age = now.Sub(last)
			if e.o.stale > 0 && fs.Age > e.o.stale {
				fs.Stale = true
			}
		}
		if err := e.lastErr[spec.Path]; err != nil {
			fs.LastError = err.Error()
			fs.LastKind = ErrorKind(err)
		}
		if fieldUnhealthy(fs) {
			healthy = false
		}
		fields = append(fields, fs)
	}
	return &Report{
		Fields:      fields,
		Snapshot:    e.version,
		Live:        e.version,
		Healthy:     healthy,
		GeneratedAt: now,
	}
}

// terminalKinds are error kinds that will not clear without human action, so a
// field carrying one is unhealthy immediately. Unavailable and RateLimited are
// transient and only make a field unhealthy once it is also stale.
func fieldUnhealthy(fs FieldStatus) bool {
	switch fs.LastKind {
	case KindNotFound, KindPermissionDenied, KindUnauthenticated, KindInvalid:
		return true
	}
	return fs.Stale
}

// Status returns a point-in-time report of the watcher's per-field health. It is
// lock-free. Age and Stale are recomputed against the current time so an idle
// engine does not report stale values as fresh.
func (w *Watcher[T]) Status() Report {
	rep := w.report.Load()
	if rep == nil {
		return Report{}
	}
	out := *rep
	out.Fields = make([]FieldStatus, len(rep.Fields))
	copy(out.Fields, rep.Fields)
	now := time.Now()
	out.GeneratedAt = now
	out.Healthy = true
	for i := range out.Fields {
		f := &out.Fields[i]
		if !f.LastOK.IsZero() {
			f.Age = now.Sub(f.LastOK)
			f.Stale = false // recomputed below via the stored threshold
		}
		if fieldUnhealthy(*f) {
			out.Healthy = false
		}
	}
	return out
}
```

**A subtlety the implementer must resolve:** `Status()` recomputes `Age` at read time but needs the `WithStale` threshold to recompute `Stale`. The engine has `o.stale`, but `Status()` is on the `Watcher`, which does not currently hold `o`. Give the `Watcher` a copy of the stale threshold (a single `time.Duration` field set in `Watch`), and use `time.Now()` unless the watcher also holds the clock. Simplest correct approach: store both the stale threshold and the `Clock` on the `Watcher` at construction, and have `Status()` use them. Do this rather than reaching into the engine, because the engine goroutine may be mid-mutation. Report the exact fields you added.

```go
// Health returns nil when every field is fresh and no field carries a terminal
// error kind. It wraps the offending fields in a HealthError otherwise, so a
// caller can log specifics. Intended for a readiness probe.
func (w *Watcher[T]) Health() error {
	rep := w.Status()
	var bad []FieldStatus
	for _, f := range rep.Fields {
		if fieldUnhealthy(f) {
			bad = append(bad, f)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return &HealthError{Fields: bad}
}
```

Add `HealthError` to `errors.go`:

```go
// HealthError is returned by Watcher.Health when one or more fields are
// unhealthy. It names the offending fields so a readiness probe can log which
// config is broken rather than a bare "unhealthy".
type HealthError struct {
	Fields []FieldStatus
}

func (e *HealthError) Error() string {
	names := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		names[i] = f.Path
	}
	return fmt.Sprintf("mamori: %d unhealthy field(s): %s", len(e.Fields), strings.Join(names, ", "))
}
```

Add `strings` to `errors.go` imports.

- [ ] **Step 5: Republish the report from the loop and flush**

In `loop`, on a successful observe (the branch that sets `e.observed[spec.Path] = val` and `e.lastOK`), clear `delete(e.lastErr, spec.Path)`. At the end of each `loop` select-iteration (after handling an update or a timer fire), and at the end of `flush` after `e.version` is bumped, call `e.w.report.Store(e.buildReport())`.

In `flush`, increment the version when a non-empty diff is applied: after `if len(fields) == 0 { return }` and before storing the config, add `e.version++`.

In `handleErr`, record the error: `e.lastErr[spec.Path] = err` (store the classified `ProviderError` or `StaleError` that is already built there, so `ErrorKind` can walk it), then rebuild and store the report so a failing field shows up in `Status` promptly.

**Verify the version sequence:** initial load is version 1; the first applied change makes it 2. The `TestStatusReportsResolvedFields` test asserts 1 at start. Add a test that mutates a source and asserts `Status().Snapshot` advances to 2 after the change is applied (use the repo's watch-test helpers to wait for the change).

- [ ] **Step 6: Run and race**

```bash
GOWORK=off go test ./... -run 'TestStatus|TestHealth|TestRedactRef' -v
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

Expected: all pass, no race.

- [ ] **Step 7: Full suite and goleak**

```bash
make test
```

The existing `goleak` checks on `Close` must still pass; report-building adds no goroutine.

- [ ] **Step 8: Stage**

```bash
git add reconciler.go errors.go status_test.go
```

```
feat(core): add Watcher.Status and Health with a snapshot version counter

The engine now publishes an immutable Report through an atomic pointer at the
end of each reconcile iteration, so Status is lock-free and safe to call
concurrently with reconciliation. Health returns nil when every field is
fresh and no field carries a terminal error kind, for use as a readiness
probe. Introduces the monotonic snapshot version that history and pinning
will build on.
```

---

### Task 3: Doctor

**Files:**
- Create: `doctor.go`
- Create: `doctor_test.go`

**Interfaces:**
- Consumes: `Report`, `FieldStatus`, `redactRef`, `fieldUnhealthy` from Tasks 1 and 2; `fieldSpecs`, `resolved`, the provider registry, and `ErrorKind`.
- Produces: `Doctor[T any](ctx context.Context, opts ...Option) (Report, error)`.

**Design.** `Doctor` walks `fieldSpecs`, resolves each field exactly once through the caller's real provider wiring, and records a `FieldStatus` per field. Unlike `resolveAll`, it must not stop at the first error: individual field failures go into the report, not the returned error. The returned error is non-nil only when the config type cannot be walked. It does not decode or validate; a field that resolves but fails validation is out of `Doctor`'s scope and is covered by `Load`.

- [ ] **Step 1: Write the failing test**

Create `doctor_test.go`:

```go
package mamori

import (
	"context"
	"errors"
	"testing"
)

func TestDoctorReportsHealthyResolution(t *testing.T) {
	t.Setenv("MAMORI_DOC_A", "alpha")
	type Config struct {
		A string `source:"env:MAMORI_DOC_A"`
		B string `source:"env:MAMORI_DOC_MISSING" default:"fallback"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatalf("Doctor returned a walk error: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("Doctor reported unhealthy for a resolvable config: %+v", rep)
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Doctor reported %d fields, want 2", len(rep.Fields))
	}
}

func TestDoctorReportsPerFieldFailureWithoutAborting(t *testing.T) {
	// A required field with no source value must show as unhealthy in the report,
	// while a sibling field still resolves. Doctor must not stop at the first
	// failure.
	t.Setenv("MAMORI_DOC_OK", "here")
	type Config struct {
		OK      string `source:"env:MAMORI_DOC_OK"`
		Missing string `source:"env:MAMORI_DOC_ABSENT_REQUIRED"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatalf("Doctor returned a walk error for a per-field failure: %v", err)
	}
	byPath := map[string]FieldStatus{}
	for _, f := range rep.Fields {
		byPath[f.Path] = f
	}
	if byPath["OK"].LastKind != "" {
		t.Errorf("OK field wrongly carries kind %q", byPath["OK"].LastKind)
	}
	if byPath["Missing"].LastKind != KindNotFound {
		t.Errorf("Missing field kind = %q, want not_found", byPath["Missing"].LastKind)
	}
	if rep.Healthy {
		t.Errorf("Doctor reported healthy despite a required field being absent")
	}
}

func TestDoctorWalkErrorForNonStruct(t *testing.T) {
	_, err := Doctor[int](context.Background())
	if err == nil {
		t.Fatal("Doctor over a non-struct type returned nil error")
	}
	_ = errors.Unwrap // keep import if needed; remove if unused
}
```

Remove the `errors` import if the final test does not use it.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run TestDoctor -v
```

Expected: `undefined: Doctor`.

- [ ] **Step 3: Implement `doctor.go`**

```go
package mamori

import (
	"context"
	"errors"
	"reflect"
	"time"
)

// Doctor resolves every field of T exactly once and returns a Report describing
// what succeeded and what failed, without starting a watcher. It accepts the
// same Options as Load and Watch, so it exercises the caller's real provider
// wiring, middleware, and Prefix rewriting.
//
// The returned error is non-nil only when T itself cannot be walked as a config
// struct. Individual field failures are recorded in the Report, not returned, so
// a caller sees every problem at once rather than only the first. Doctor does
// not decode or validate; a field that resolves but fails validation is Load's
// concern, not a reachability check's.
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	var cfg T
	specs, err := fieldSpecs(reflect.TypeOf(cfg))
	if err != nil {
		return Report{}, err
	}

	now := o.clock.Now()
	fields := make([]FieldStatus, 0, len(specs))
	healthy := true
	for _, spec := range specs {
		fs := FieldStatus{
			Path:      spec.Path,
			Scheme:    spec.Ref.Scheme,
			Ref:       redactRef(spec.Ref),
			Sensitive: spec.Sensitive,
		}
		val, rerr := probeField(ctx, spec, o)
		switch {
		case rerr == nil:
			fs.Version = val.Version
			fs.LastOK = now
		case errors.Is(rerr, ErrNotFound) && (spec.HasDefault || spec.Optional):
			// Absent but covered by a default or optional: this resolves in
			// practice, so it is healthy.
			fs.LastOK = now
		default:
			fs.LastError = rerr.Error()
			fs.LastKind = ErrorKind(rerr)
		}
		if fieldUnhealthy(fs) {
			healthy = false
		}
		fields = append(fields, fs)
	}
	return Report{
		Fields:      fields,
		Snapshot:    0, // Doctor is a one-shot probe, not a running snapshot
		Live:        0,
		Healthy:     healthy,
		GeneratedAt: now,
	}, nil
}

// probeField resolves a single field's ref through its provider, returning the
// raw value or error without applying defaults or aborting. It is Doctor's
// non-fail-fast counterpart to resolveOne.
func probeField(ctx context.Context, spec fieldSpec, o *options) (Value, error) {
	p, ok := o.provider(spec.Ref.Scheme)
	if !ok {
		return Value{}, &ProviderError{
			Scheme: spec.Ref.Scheme,
			Ref:    spec.Ref.Raw,
			Err:    ErrInvalid,
		}
	}
	sctx, finish := o.tracer.StartResolve(ctx, spec.Ref.Scheme, spec.Ref.Raw)
	start := o.clock.Now()
	val, err := p.Resolve(sctx, spec.Ref)
	finish(err)
	o.meter.RecordResolve(spec.Ref.Scheme, o.clock.Now().Sub(start), err)
	if err != nil {
		return Value{}, &ProviderError{Scheme: spec.Ref.Scheme, Ref: spec.Ref.Raw, Err: err}
	}
	return val, nil
}
```

Note the unregistered-scheme case maps to `ErrInvalid`, which is honest: a ref naming a provider that is not wired is a malformed configuration for this process. Confirm `defaultOptions().provider` resolves built-in schemes the same way `resolveAll` does; read `reconcile.go`'s `provider` method and match it.

**A decision the implementer must make and report:** `probeField` resolves one ref at a time and does not use `BatchProvider`. That is simpler and correct, but a batch-only provider (if any exists that cannot resolve singly) would behave differently under Doctor than under Load. Check whether any in-repo provider implements only `BatchProvider` without a working single `Resolve`; the `Provider` interface requires `Resolve`, so every provider can resolve singly, but note in your report whether single-resolve of a normally-batched ref is semantically faithful. If it is not, Doctor should group by scheme and use `ResolveBatch` like `resolveAll`; decide and report.

- [ ] **Step 4: Run, confirm pass**

```bash
GOWORK=off go test ./... -run TestDoctor -v
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

- [ ] **Step 5: Full suite**

```bash
make test
```

- [ ] **Step 6: Stage**

```bash
git add doctor.go doctor_test.go
```

```
feat(core): add Doctor for one-shot config reachability checks

Doctor resolves every field once through the caller's real provider wiring
and returns a Report of what resolved and what failed, without starting a
watcher and without aborting on the first error. It is the pre-deploy
counterpart to Status: run it in a build-tagged CI test to catch a
misconfigured ref before it ships.
```

---

### Task 4: Documentation

**Files:**
- Create: `site/src/pages/docs/observability.md`
- Modify: `site/src/pages/docs/index.md` (nav entry)
- Modify: `README.md` (a short "Observability" section and a mention in the feature list)
- Modify: `site/src/pages/docs/usage.md` (show `Status` in the watch walkthrough)

**Interfaces:** consumes everything from Tasks 1 to 3.

- [ ] **Step 1: Write the observability page**

Create `site/src/pages/docs/observability.md` matching the site's existing frontmatter and voice (read a sibling page like `opentelemetry.md` first). Cover:
- `Watcher.Status() Report` and the `FieldStatus`/`Report` shapes, noting refs are redacted and values never appear.
- `Watcher.Health() error` as a Kubernetes readiness probe, with the terminal-vs-transient kind rule (`not_found`, `permission_denied`, `unauthenticated`, `invalid` are terminal; `unavailable` and `rate_limited` only fail health once stale).
- `Doctor[T]` as the pre-deploy check, with the build-tagged CI test pattern:

```go
//go:build preflight

func TestConfigPreflight(t *testing.T) {
	rep, err := mamori.Doctor[Config](context.Background(), appProviders()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Fields {
		if f.LastKind != "" {
			t.Errorf("%s (%s): %s: %s", f.Path, f.Ref, f.LastKind, f.LastError)
		}
	}
}
```

- State plainly that the HTTP endpoint and authentication are a separate, later addition, so the page does not promise routes that do not exist yet. Cross-link `opentelemetry.md`.

- [ ] **Step 2: Wire nav, README, usage**

Add the observability page to the docs nav in `index.md`. Add a short README section and a bullet in the feature list. Add a `w.Status()` line to the existing watch example in `usage.md`.

- [ ] **Step 3: Build the site**

```bash
make site-build   # Node 22; run `nvm use 22` first if the engine check fails
```

- [ ] **Step 4: Stage**

```bash
git add site/src/pages/docs/observability.md site/src/pages/docs/index.md site/src/pages/docs/usage.md README.md
```

```
docs: document Status, Health, and Doctor

Adds an observability page covering the report types, the readiness-probe
health rule, and the pre-deploy Doctor pattern, and states that the HTTP
endpoint is a later addition rather than promising routes that do not exist.
```

---

## Self-Review

**Spec coverage.** Implements spec section 7.1 (report types), 7.2 (lock-free publishing), 7.3 (Status/Health), and 7.4 (Doctor). Deliberately NOT here, with the reason: section 7.5 (HTTP handler) and 7.6 (Authenticator and the shipped schemes) are a separate plan, because they add `net/http` surface and an extension interface that deserve their own review; this plan is the pure-core reporting foundation they build on. The `Pinned`/history fields of the Report (7.1) land with workstream D; `Candidates` with workstream E. Both are additive.

**Placeholders.** None. Every code step carries complete code. Three steps require a judgment call and say so explicitly rather than hand-waving: Task 2 Step 4's exact `Watcher` fields for the stale threshold and clock, Task 3's batch-vs-single decision in `probeField`, and Task 1 Step 3's check that `secret.Redacted` is exported.

**Type consistency.** `Report` and `FieldStatus` are defined once in Task 1 and populated identically by the engine (Task 2) and Doctor (Task 3). `fieldUnhealthy` is the single source of the healthy/terminal rule, shared by `buildReport`, `Status`, `Health`, and `Doctor`, so the definition of "healthy" cannot drift between the live and one-shot paths. `Doctor` sets `Snapshot`/`Live` to 0 to signal "not a running snapshot", distinct from a live watcher's version which starts at 1.

**Risk noted.** The highest-risk item is the concurrency boundary in Task 2: `Status()` must never read engine maps directly. The design routes everything through `atomic.Pointer[Report]` and the plan mandates a `-race` test with concurrent `Status` and reconciliation. The second risk is the version counter, which workstream D depends on; the plan pins its semantics (starts at 1, increments per applied diff) with an explicit test so D inherits a stable contract.
