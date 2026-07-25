# Error Classification Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give mamori a provider-independent way to classify a resolve failure, so "the secret does not exist" and "this role may not read it" stop collapsing into one opaque error.

**Architecture:** Extend the existing sentinel pattern in `errors.go` rather than invent a parallel mechanism. `ErrNotFound` already works this way and keeps its unique behavior (the only kind that triggers defaults). Five new sentinels join it, `ErrorKind` classifies by walking the `errors.Is` chain, and `SentinelFor` inverts that so a classification can be reconstructed from a wire value later. The conformance kit gains a hook that proves providers do not flatten the chain.

**Tech Stack:** Go 1.26, stdlib `errors`, `go.opentelemetry.io/otel` (in `x/otel` only).

This is plan 1 of 7 from `docs/superpowers/specs/2026-07-24-operational-layer-design.md` (workstream A). It gates all six others: `Status`, `Doctor`, the CLI exit codes, and the server wire protocol all report kinds.

## Global Constraints

- **Core dependencies are frozen.** `github.com/xavidop/mamori` may import only stdlib, `github.com/go-playground/validator/v10`, `github.com/go-viper/mapstructure/v2`, `github.com/fsnotify/fsnotify`, `gopkg.in/yaml.v3`, and `go.uber.org/goleak` (test-only). This plan adds no dependency to any module.
- **Do not run `git commit`.** This project's owner handles all git operations. Commit steps below give the exact message to use; stage the files and report the suggested message, then stop. Never run `git commit` yourself.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for all test commands.** Modules are tested independently, matching CI. The `Makefile` sets this; when running `go test` directly in a module directory, prefix with `GOWORK=off`.
- **The tree stays green after every task.** `make test` must pass at every commit point. `providertest.Config.Fail` is therefore optional in this plan and becomes required in plan 2, after all 31 providers supply it.
- **No em-dash characters in any prose you write**, in code comments, docs, or commit messages.
- Doc comments on every exported symbol, matching the existing style in `errors.go`: explain the why, not just the what.

---

### Task 1: Kind type, sentinels, and ErrorKind

**Files:**
- Modify: `errors.go` (append after the `ErrNotFound` block, currently lines 8-12)
- Test: `errors_test.go` (append; file currently holds `TestProviderErrorIsNotFound` and `TestValidationErrorUnwrap`)

**Interfaces:**
- Consumes: nothing from earlier tasks. `ErrNotFound` and `ProviderError` already exist in `errors.go`.
- Produces: `mamori.Kind` (string type) with constants `KindNotFound`, `KindPermissionDenied`, `KindUnauthenticated`, `KindUnavailable`, `KindRateLimited`, `KindInvalid`, `KindUnknown`; sentinels `ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`; functions `ErrorKind(err error) Kind` and `SentinelFor(k Kind) error`. Task 2 uses `ErrorKind` and every sentinel. Task 3 uses `ErrorKind`. Plans 2 through 7 use all of it.

- [ ] **Step 1: Write the failing test**

Append to `errors_test.go`. Note the existing file already imports `errors` and `testing`; add `fmt` to the import block.

```go
func TestErrorKindClassifiesSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotFound", mamori.ErrNotFound, mamori.KindNotFound},
		{"PermissionDenied", mamori.ErrPermissionDenied, mamori.KindPermissionDenied},
		{"Unauthenticated", mamori.ErrUnauthenticated, mamori.KindUnauthenticated},
		{"Unavailable", mamori.ErrUnavailable, mamori.KindUnavailable},
		{"RateLimited", mamori.ErrRateLimited, mamori.KindRateLimited},
		{"Invalid", mamori.ErrInvalid, mamori.KindInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mamori.ErrorKind(tc.err); got != tc.want {
				t.Fatalf("ErrorKind(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorKindNilIsEmpty(t *testing.T) {
	if got := mamori.ErrorKind(nil); got != "" {
		t.Fatalf("ErrorKind(nil) = %q, want empty string", got)
	}
}

func TestErrorKindUnrecognizedIsUnknown(t *testing.T) {
	if got := mamori.ErrorKind(errors.New("something odd")); got != mamori.KindUnknown {
		t.Fatalf("ErrorKind(unrecognized) = %q, want %q", got, mamori.KindUnknown)
	}
}

func TestErrorKindThroughWrapping(t *testing.T) {
	// The %w verb is how providers are told to classify. If a provider uses %v
	// instead, the chain breaks and this is the behavior that catches it.
	wrapped := fmt.Errorf("secretsmanager: %w: AccessDeniedException", mamori.ErrPermissionDenied)
	if got := mamori.ErrorKind(wrapped); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(wrapped) = %q, want %q", got, mamori.KindPermissionDenied)
	}

	flattened := fmt.Errorf("secretsmanager: %v: AccessDeniedException", mamori.ErrPermissionDenied)
	if got := mamori.ErrorKind(flattened); got != mamori.KindUnknown {
		t.Fatalf("ErrorKind(flattened with %%v) = %q, want %q", got, mamori.KindUnknown)
	}
}

func TestErrorKindThroughProviderError(t *testing.T) {
	pe := &mamori.ProviderError{
		Scheme: "aws-sm",
		Ref:    "aws-sm://prod/db#password",
		Err:    fmt.Errorf("%w: denied by policy", mamori.ErrPermissionDenied),
	}
	if got := mamori.ErrorKind(pe); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(ProviderError) = %q, want %q", got, mamori.KindPermissionDenied)
	}
}

func TestErrorKindThroughJoin(t *testing.T) {
	joined := errors.Join(errors.New("first"), mamori.ErrRateLimited)
	if got := mamori.ErrorKind(joined); got != mamori.KindRateLimited {
		t.Fatalf("ErrorKind(joined) = %q, want %q", got, mamori.KindRateLimited)
	}
}

func TestErrorKindNotFoundWinsOverOthers(t *testing.T) {
	// NotFound is the only kind that drives behavior (defaults and optional
	// handling), so when an error somehow carries two sentinels it must win.
	both := errors.Join(mamori.ErrUnavailable, mamori.ErrNotFound)
	if got := mamori.ErrorKind(both); got != mamori.KindNotFound {
		t.Fatalf("ErrorKind(NotFound+Unavailable) = %q, want %q", got, mamori.KindNotFound)
	}
}

func TestSentinelForRoundTrips(t *testing.T) {
	kinds := []mamori.Kind{
		mamori.KindNotFound, mamori.KindPermissionDenied, mamori.KindUnauthenticated,
		mamori.KindUnavailable, mamori.KindRateLimited, mamori.KindInvalid,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			sentinel := mamori.SentinelFor(k)
			if sentinel == nil {
				t.Fatalf("SentinelFor(%q) returned nil", k)
			}
			if got := mamori.ErrorKind(sentinel); got != k {
				t.Fatalf("ErrorKind(SentinelFor(%q)) = %q, want %q", k, got, k)
			}
		})
	}
}

func TestSentinelForUnknownIsNil(t *testing.T) {
	if got := mamori.SentinelFor(mamori.KindUnknown); got != nil {
		t.Fatalf("SentinelFor(KindUnknown) = %v, want nil", got)
	}
	if got := mamori.SentinelFor(""); got != nil {
		t.Fatalf("SentinelFor(\"\") = %v, want nil", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOWORK=off go test ./... -run TestErrorKind -v
```

Expected: compile failure, `undefined: mamori.Kind`, `undefined: mamori.ErrPermissionDenied`, `undefined: mamori.ErrorKind`.

- [ ] **Step 3: Write the implementation**

Append to `errors.go`, after the existing `ErrNotFound` declaration:

```go
// Kind is a coarse, provider-independent classification of a resolve failure.
// It exists so diagnostics can distinguish conditions that need human action
// (a missing secret, a denied permission) from transient ones (an unreachable
// backend, a throttled request). Providers produce it by wrapping one of the
// sentinels below; consumers read it with ErrorKind.
type Kind string

const (
	KindNotFound         Kind = "not_found"
	KindPermissionDenied Kind = "permission_denied"
	KindUnauthenticated  Kind = "unauthenticated"
	KindUnavailable      Kind = "unavailable"
	KindRateLimited      Kind = "rate_limited"
	KindInvalid          Kind = "invalid"
	// KindUnknown is the honest answer for an error a provider cannot map. It is
	// a legal outcome, not a failure: a provider that guesses is worse than one
	// that admits it does not know.
	KindUnknown Kind = "unknown"
)

// Classification sentinels. Providers wrap the underlying SDK error with the
// matching sentinel using %w, which preserves errors.As access to the original:
//
//	return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)
//
// Only ErrNotFound changes mamori's behavior (it is what triggers `default:` and
// `optional` handling). The rest are diagnostic.
var (
	ErrPermissionDenied = errors.New("mamori: permission denied")
	ErrUnauthenticated  = errors.New("mamori: unauthenticated")
	ErrUnavailable      = errors.New("mamori: unavailable")
	ErrRateLimited      = errors.New("mamori: rate limited")
	ErrInvalid          = errors.New("mamori: invalid reference")
)

// kindSentinels pairs each Kind with its sentinel, in the order ErrorKind tests
// them. ErrNotFound is first so it wins when an error carries more than one
// sentinel, since it is the only kind that drives resolution behavior.
var kindSentinels = [...]struct {
	kind Kind
	err  error
}{
	{KindNotFound, ErrNotFound},
	{KindPermissionDenied, ErrPermissionDenied},
	{KindUnauthenticated, ErrUnauthenticated},
	{KindUnavailable, ErrUnavailable},
	{KindRateLimited, ErrRateLimited},
	{KindInvalid, ErrInvalid},
}

// ErrorKind classifies err by walking its errors.Is chain. It returns the empty
// Kind for a nil error and KindUnknown for an error carrying no sentinel.
//
// An error that lost its chain (a provider that formatted with %v rather than
// %w) reports KindUnknown. That is the failure the providertest conformance
// case exists to catch.
func ErrorKind(err error) Kind {
	if err == nil {
		return ""
	}
	for _, ks := range kindSentinels {
		if errors.Is(err, ks.err) {
			return ks.kind
		}
	}
	return KindUnknown
}

// SentinelFor returns the sentinel error corresponding to k, or nil for
// KindUnknown and the empty Kind, neither of which has one.
//
// It is the inverse of ErrorKind, and exists so a classification that arrived
// as a value rather than an error (over the wire, from a config file) can be
// turned back into an error that errors.Is matches.
func SentinelFor(k Kind) error {
	for _, ks := range kindSentinels {
		if ks.kind == k {
			return ks.err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOWORK=off go test ./... -run 'TestErrorKind|TestSentinelFor' -v
```

Expected: all subtests PASS.

- [ ] **Step 5: Run the full core suite and the race detector**

```bash
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

Expected: all PASS. No existing test should change behavior; this task only adds symbols.

- [ ] **Step 6: Stage and report the commit message**

```bash
git add errors.go errors_test.go
```

Suggested message (do not run `git commit`, report it to the user):

```
feat(core): add error classification kinds and ErrorKind

Adds Kind plus five sentinels alongside the existing ErrNotFound, so a
resolve failure can be told apart as denied, unauthenticated, unavailable,
rate limited, or invalid. ErrorKind walks the errors.Is chain; SentinelFor
inverts it for classifications that arrive as values rather than errors.

ErrNotFound keeps its unique behavior as the only kind that triggers
defaults, and wins when an error carries more than one sentinel.
```

---

### Task 2: providertest Fail hook and the ErrorClassification case

**Files:**
- Modify: `providertest/providertest.go` (add two fields to `Config` after `SkipWatch` at line 52; add a subtest to `Run` at lines 83-93; add the case function)
- Test: `providertest/providertest_test.go` (append)

**Interfaces:**
- Consumes: `mamori.ErrorKind`, `mamori.Kind`, and all six sentinels from Task 1.
- Produces: `providertest.Config.Fail func(ctx context.Context, key string, err error) error` and `providertest.Config.Clear func(ctx context.Context, key string) error`. Plan 2 implements both on all 31 provider fakes and then makes them required.

**Why the hook is optional here:** making it required now would fail the conformance test in all 31 provider modules the moment this lands. It becomes required in the final task of plan 2, once every module supplies it.

- [ ] **Step 1: Write the failing test**

Append to `providertest/providertest_test.go`:

```go
// classifyingProvider is a minimal provider whose backend can be told to fail
// with a specific error. It models a well-behaved provider: it wraps with %w.
type classifyingProvider struct {
	mu     sync.Mutex
	values map[string]string
	fails  map[string]error
}

func newClassifyingProvider() *classifyingProvider {
	return &classifyingProvider{values: map[string]string{}, fails: map[string]error{}}
}

func (p *classifyingProvider) Scheme() string { return "classify" }

func (p *classifyingProvider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.fails[ref.Path]; ok {
		return mamori.Value{}, fmt.Errorf("classify backend: %w", err)
	}
	v, ok := p.values[ref.Path]
	if !ok {
		return mamori.Value{}, fmt.Errorf("%w: %s", mamori.ErrNotFound, ref.Path)
	}
	return mamori.Value{Bytes: []byte(v), Version: mamori.VersionHash([]byte(v))}, nil
}

func (p *classifyingProvider) set(key, val string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[key] = val
}

func (p *classifyingProvider) fail(key string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[key] = err
}

func (p *classifyingProvider) clear(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.fails, key)
}

// flatteningProvider is the bug the conformance case must catch: it formats the
// sentinel with %v, destroying the errors.Is chain.
type flatteningProvider struct{ *classifyingProvider }

func (p flatteningProvider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.fails[ref.Path]; ok {
		return mamori.Value{}, fmt.Errorf("classify backend: %v", err)
	}
	v, ok := p.values[ref.Path]
	if !ok {
		return mamori.Value{}, fmt.Errorf("%w: %s", mamori.ErrNotFound, ref.Path)
	}
	return mamori.Value{Bytes: []byte(v), Version: mamori.VersionHash([]byte(v))}, nil
}

func TestErrorClassificationPassesForWrappingProvider(t *testing.T) {
	backend := newClassifyingProvider()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return backend },
		Ref:    func(key string) string { return "classify://" + key },
		Seed:   func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Fail:   func(_ context.Context, key string, err error) error { backend.fail(key, err); return nil },
		Clear:  func(_ context.Context, key string) error { backend.clear(key); return nil },
	})
}

func TestErrorClassificationFailsForFlatteningProvider(t *testing.T) {
	backend := newClassifyingProvider()
	flat := flatteningProvider{backend}

	// Run the case against a recording TB rather than t, so the expected failure
	// does not fail this test.
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:   func() mamori.Provider { return flat },
		Ref:   func(key string) string { return "classify://" + key },
		Seed:  func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Fail:  func(_ context.Context, key string, err error) error { backend.fail(key, err); return nil },
		Clear: func(_ context.Context, key string) error { backend.clear(key); return nil },
	})

	if !fake.failed {
		t.Fatal("ErrorClassification passed a provider that flattens errors with %v; it must fail")
	}
}

func TestErrorClassificationSkippedWithoutFailHook(t *testing.T) {
	backend := newClassifyingProvider()
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:  func() mamori.Provider { return backend },
		Ref:  func(key string) string { return "classify://" + key },
		Seed: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		// no Fail hook
	})
	if fake.failed {
		t.Fatal("ErrorClassification failed when no Fail hook was supplied; it must skip")
	}
	if !fake.skipped {
		t.Fatal("ErrorClassification did not skip when no Fail hook was supplied")
	}
}

// recordingTB captures whether the suite failed or skipped, without failing the
// enclosing test.
type recordingTB struct {
	testing.TB
	failed  bool
	skipped bool
}

func (r *recordingTB) Errorf(format string, args ...any) { r.failed = true }
func (r *recordingTB) Fatalf(format string, args ...any) { r.failed = true; panic(errSuiteFailed) }
func (r *recordingTB) Fatal(args ...any)                 { r.failed = true; panic(errSuiteFailed) }
func (r *recordingTB) Error(args ...any)                 { r.failed = true }
func (r *recordingTB) Skip(args ...any)                  { r.skipped = true; panic(errSuiteSkipped) }
func (r *recordingTB) SkipNow()                          { r.skipped = true; panic(errSuiteSkipped) }
func (r *recordingTB) Helper()                           {}

var (
	errSuiteFailed  = errors.New("suite failed")
	errSuiteSkipped = errors.New("suite skipped")
)
```

Add to the test file's imports: `context`, `errors`, `fmt`, `sync`, `testing`, `github.com/xavidop/mamori`, `github.com/xavidop/mamori/providertest`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd providertest && GOWORK=off go test ./... -run TestErrorClassification -v
```

Expected: compile failure, `unknown field Fail in struct literal`, `undefined: providertest.RunErrorClassification`.

- [ ] **Step 3: Add the Config fields**

In `providertest/providertest.go`, add after the `SkipWatch` field (line 52):

```go
	// Fail makes the backend return err for key, on the next Resolve and on any
	// active Watch, until Clear is called for that key. It powers the
	// ErrorClassification case, which verifies the provider preserves a
	// classified error's errors.Is chain rather than flattening it.
	//
	// The common real bug this catches is a provider that catches an error and
	// reformats it with fmt.Errorf("...: %v", err), which destroys the chain and
	// makes every failure report KindUnknown.
	//
	// If nil, ErrorClassification is skipped. It becomes required once every
	// in-repo provider supplies it.
	Fail func(ctx context.Context, key string, err error) error

	// Clear cancels a Fail for key. Required whenever Fail is set.
	Clear func(ctx context.Context, key string) error
```

- [ ] **Step 4: Add the conformance case**

In `providertest/providertest.go`, add the exported entry point and the case body:

```go
// RunErrorClassification runs only the error-classification case. Run calls it;
// it is exported so the kit can test itself against a deliberately broken
// provider without running the whole suite.
func RunErrorClassification(tb testing.TB, c Config) {
	tb.Helper()
	if c.Fail == nil {
		tb.Skip("providertest: no Fail hook supplied; skipping ErrorClassification")
		return
	}
	if c.Clear == nil {
		tb.Fatal("providertest: Config.Clear is required whenever Config.Fail is set")
		return
	}

	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"PermissionDenied", mamori.ErrPermissionDenied, mamori.KindPermissionDenied},
		{"Unauthenticated", mamori.ErrUnauthenticated, mamori.KindUnauthenticated},
		{"Unavailable", mamori.ErrUnavailable, mamori.KindUnavailable},
		{"RateLimited", mamori.ErrRateLimited, mamori.KindRateLimited},
		{"Invalid", mamori.ErrInvalid, mamori.KindInvalid},
	}

	for _, tc := range cases {
		key := c.key("classify-" + strings.ToLower(tc.name))
		if err := c.Seed(ctx, key, "seeded-value"); err != nil {
			tb.Fatalf("Seed(%q): %v", key, err)
			return
		}
		if err := c.Fail(ctx, key, tc.err); err != nil {
			tb.Fatalf("Fail(%q): %v", key, err)
			return
		}

		p := c.New()
		ref, err := mamori.ParseRef(c.Ref(key))
		if err != nil {
			tb.Fatalf("Ref(%q) produced an unparseable ref: %v", key, err)
			return
		}
		_, resolveErr := p.Resolve(ctx, ref)

		if cerr := c.Clear(ctx, key); cerr != nil {
			tb.Fatalf("Clear(%q): %v", key, cerr)
			return
		}

		if resolveErr == nil {
			tb.Fatalf("%s: Resolve returned a nil error while the backend was failing", tc.name)
			return
		}
		if got := mamori.ErrorKind(resolveErr); got != tc.want {
			tb.Fatalf("%s: ErrorKind(err) = %q, want %q. The provider most likely "+
				"formatted the error with %%v instead of %%w, which breaks the "+
				"errors.Is chain. Underlying error: %v",
				tc.name, got, tc.want, resolveErr)
			return
		}
	}
}
```

Add `strings` to the file's import block.

Note the explicit `return` after every `tb.Fatalf`. The real `*testing.T` stops the goroutine at `Fatalf`, but `recordingTB` in the self-test does not, so the returns keep the function honest under both.

- [ ] **Step 5: Register the case in Run**

In `providertest/providertest.go`, add to `Run`'s subtest list, after the `NotFoundTyped` line:

```go
	t.Run("ErrorClassification", func(t *testing.T) { RunErrorClassification(t, c) })
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd providertest && GOWORK=off go test ./... -v
```

Expected: `TestErrorClassificationPassesForWrappingProvider` PASS (including its `ErrorClassification` subtest), `TestErrorClassificationFailsForFlatteningProvider` PASS, `TestErrorClassificationSkippedWithoutFailHook` PASS.

- [ ] **Step 7: Verify every existing provider still passes**

This is the step that proves the hook is genuinely optional and the tree is still green.

```bash
make test
```

Expected: every module PASS. Each provider's `ErrorClassification` subtest reports SKIP, since none supply `Fail` yet.

- [ ] **Step 8: Stage and report the commit message**

```bash
git add providertest/providertest.go providertest/providertest_test.go
```

Suggested message:

```
feat(providertest): add Fail hook and ErrorClassification case

Injects each classification sentinel through a new Config.Fail hook and
asserts the kind survives Resolve unchanged. This catches the common bug of
a provider reformatting an error with %v, which silently destroys the
errors.Is chain and makes every failure report as unknown.

The hook is optional for now so existing providers stay green; it becomes
required once all 31 modules supply it.
```

---

### Task 3: Record the error kind in x/otel

**Files:**
- Modify: `x/otel/otel.go` (add an attribute constant near line 55; extend `RecordResolve` at lines 126-140 and the tracer's span-end path near line 181)
- Test: `x/otel/otel_test.go` (append)

**Interfaces:**
- Consumes: `mamori.ErrorKind` from Task 1.
- Produces: the attribute key `mamori.error.kind` on the `mamori.resolve.duration` histogram and the `mamori.resolve` span. No Go symbols other modules depend on.

**Scope note:** `mamori.watch.errors` does not gain the attribute. `Meter.RecordWatchError(scheme string)` (`observ.go:12`) takes no error, so carrying a kind there would require a breaking change to the `Meter` interface. Deliberately deferred per the spec.

- [ ] **Step 1: Write the failing test**

Append to `x/otel/otel_test.go`. Follow the existing test file's setup for an in-memory metric reader and tracer; if it already has a helper that collects recorded metrics, reuse it rather than writing a second one.

```go
func TestRecordResolveTagsErrorKind(t *testing.T) {
	reader := metricsdk.NewManualReader()
	mp := metricsdk.NewMeterProvider(metricsdk.WithReader(reader))
	m, err := otelbridge.NewMeter(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	m.RecordResolve("aws-sm", 5*time.Millisecond,
		fmt.Errorf("%w: denied by policy", mamori.ErrPermissionDenied))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	got := findAttr(t, &rm, otelbridge.MetricResolveDuration, "mamori.error.kind")
	if got != string(mamori.KindPermissionDenied) {
		t.Fatalf("mamori.error.kind = %q, want %q", got, mamori.KindPermissionDenied)
	}
}

func TestRecordResolveOmitsErrorKindOnSuccess(t *testing.T) {
	reader := metricsdk.NewManualReader()
	mp := metricsdk.NewMeterProvider(metricsdk.WithReader(reader))
	m, err := otelbridge.NewMeter(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	m.RecordResolve("env", time.Millisecond, nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	if got := findAttr(t, &rm, otelbridge.MetricResolveDuration, "mamori.error.kind"); got != "" {
		t.Fatalf("successful resolve carried mamori.error.kind = %q, want no attribute", got)
	}
}

// findAttr returns the string value of the named attribute on the first data
// point of the named metric, or "" when the attribute is absent.
func findAttr(t *testing.T, rm *metricdata.ResourceMetrics, metricName, attrKey string) string {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != metricName {
				continue
			}
			hist, ok := mt.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q is %T, want Histogram[float64]", metricName, mt.Data)
			}
			for _, dp := range hist.DataPoints {
				if v, found := dp.Attributes.Value(attribute.Key(attrKey)); found {
					return v.AsString()
				}
			}
			return ""
		}
	}
	t.Fatalf("metric %q not found", metricName)
	return ""
}
```

Imports needed in the test file: `context`, `fmt`, `testing`, `time`, `go.opentelemetry.io/otel/attribute`, `go.opentelemetry.io/otel/sdk/metric` as `metricsdk`, `go.opentelemetry.io/otel/sdk/metric/metricdata`, `github.com/xavidop/mamori`, and the package under test. Match the alias the existing tests use for the package under test; the plan writes it as `otelbridge`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd x/otel && GOWORK=off go test ./... -run TestRecordResolve -v
```

Expected: FAIL. `TestRecordResolveTagsErrorKind` reports `mamori.error.kind = "", want "permission_denied"`.

- [ ] **Step 3: Add the attribute constant**

In `x/otel/otel.go`, add to the metric attribute key block (near line 55):

```go
	// attrErrorKind carries the mamori.Kind classification of a failed resolve.
	// It is absent on success rather than set to an empty or "none" value, so
	// the attribute's presence alone selects failures.
	attrErrorKind = "mamori.error.kind"
```

- [ ] **Step 4: Tag the histogram**

Replace the body of `RecordResolve` in `x/otel/otel.go`:

```go
// RecordResolve records the resolve duration (in milliseconds) tagged with the
// scheme and a status of "ok" or "error". A failed resolve additionally carries
// mamori.error.kind, so a dashboard can separate a denied permission from a
// throttled request without parsing error strings.
func (m *meter) RecordResolve(scheme string, dur time.Duration, err error) {
	attrs := []attribute.KeyValue{
		attribute.String(attrScheme, scheme),
		attribute.String(attrStatus, statusOK),
	}
	if err != nil {
		attrs[1] = attribute.String(attrStatus, statusError)
		attrs = append(attrs, attribute.String(attrErrorKind, string(mamori.ErrorKind(err))))
	}
	m.resolveDuration.Record(
		m.ctx,
		float64(dur)/float64(time.Millisecond),
		metric.WithAttributes(attrs...),
	)
}
```

- [ ] **Step 5: Tag the span**

In the tracer adapter, find the span-end path that currently calls `span.SetStatus(codes.Error, err.Error())` (near line 181) and add the attribute immediately before it:

```go
		if err != nil {
			span.SetAttributes(attribute.String(attrErrorKind, string(mamori.ErrorKind(err))))
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd x/otel && GOWORK=off go test ./... -v
GOWORK=off go test -race ./...
```

Expected: all PASS.

- [ ] **Step 7: Stage and report the commit message**

```bash
git add x/otel/otel.go x/otel/otel_test.go
```

Suggested message:

```
feat(otel): tag resolve metrics and spans with mamori.error.kind

A failed resolve now carries its classification, so a dashboard can split a
denied permission from a throttled request without parsing error strings.
The attribute is absent on success rather than set to a placeholder, so its
presence alone selects failures.

watch.errors is unchanged: Meter.RecordWatchError takes no error, and
threading one through would break the Meter interface for marginal value.
```

---

### Task 4: Document the classification contract

**Files:**
- Modify: `site/src/pages/docs/writing-a-provider.md`
- Modify: `CONTRIBUTING.md`
- Modify: `site/src/pages/docs/concepts.md`
- Modify: `CHANGELOG.md` is generated by semantic-release from commit messages; do **not** hand-edit it.

**Interfaces:**
- Consumes: everything from Tasks 1 through 3.
- Produces: the reference the 31 sweep tasks in plan 2 follow.

- [ ] **Step 1: Add the error-mapping section to the provider guide**

Add a new section to `site/src/pages/docs/writing-a-provider.md`, placed after the existing not-found guidance so it reads as an extension of it. Match the page's existing heading level and prose voice.

````markdown
## Classifying errors

`ErrNotFound` tells mamori a value is absent, which is what triggers `default:`
and `optional` handling. Every other failure should also be classified, so that
`Doctor`, `Status`, and the CLI can tell an operator what is actually wrong
instead of printing an opaque provider error.

Wrap the SDK error with the matching sentinel using `%w`:

```go
var ae smithy.APIError
if errors.As(err, &ae) {
    switch ae.ErrorCode() {
    case "ResourceNotFoundException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrNotFound, err)
    case "AccessDeniedException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)
    case "ThrottlingException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrRateLimited, err)
    }
}
return mamori.Value{}, err // unmapped: reports as unknown, which is fine
```

Use `%w` for the sentinel and `%v` for the SDK error, in that order. That keeps
`errors.Is(err, mamori.ErrPermissionDenied)` working for mamori while leaving
`errors.As` able to reach the original SDK error type for anyone who wants it.

### Which kind to use

| Kind | Use for |
|---|---|
| `ErrNotFound` | Key, secret, path, or version genuinely absent |
| `ErrPermissionDenied` | Authenticated but not authorized: IAM deny, Vault policy, RBAC |
| `ErrUnauthenticated` | Missing, malformed, or expired credentials; failed token renewal |
| `ErrUnavailable` | Network failure, DNS, timeout, 5xx, circuit open |
| `ErrRateLimited` | Throttling, quota exhaustion, 429 |
| `ErrInvalid` | The ref is malformed for this provider, or the payload cannot be parsed |
| (unmapped) | Anything else. Reports as `unknown`, which is an honest answer. |

Leaving an error unmapped is fine. Guessing is not: a provider that reports
`permission_denied` for a network timeout sends an operator down the wrong path.

### The mistake to avoid

```go
// WRONG: %v flattens the sentinel into a string and destroys the chain.
return mamori.Value{}, fmt.Errorf("secretsmanager: %v", mamori.ErrPermissionDenied)
```

Everything still compiles and the message still reads correctly, but
`errors.Is` no longer matches and every failure reports as `unknown`. The
`ErrorClassification` conformance case exists to catch exactly this.

### Wiring the conformance case

Supply `Fail` and `Clear` on your `providertest.Config` so the case can run.
They make your fake backend return a given error for a key, and stop:

```go
providertest.Run(t, providertest.Config{
    New:    func() mamori.Provider { return newWithClient(fake) },
    Ref:    func(key string) string { return "myscheme://" + key },
    Seed:   func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
    Mutate: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
    Fail:   func(_ context.Context, key string, err error) error { fake.fail(key, err); return nil },
    Clear:  func(_ context.Context, key string) error { fake.clear(key); return nil },
})
```

The case checks that a classified error survives your `Resolve` unchanged. It
does not check your SDK mapping, which no in-memory fake can exercise; cover
that with a table test over real SDK error values.
````

- [ ] **Step 2: Add the concepts entry**

Add a short subsection to `site/src/pages/docs/concepts.md` where errors are discussed, so consumers (not just provider authors) learn the vocabulary:

```markdown
### Error kinds

Every resolve failure carries a coarse classification you can read with
`mamori.ErrorKind(err)`: `not_found`, `permission_denied`, `unauthenticated`,
`unavailable`, `rate_limited`, `invalid`, or `unknown`.

Only `not_found` changes behavior, since it is what triggers a field's
`default:` or `optional` handling. The rest are diagnostic, and are what
`Doctor` and the status endpoint report so an operator can tell a
misconfiguration from an outage.

Match them with `errors.Is`:

```go
if errors.Is(err, mamori.ErrPermissionDenied) {
    // the credential is fine, the authorization is not
}
```
```

- [ ] **Step 3: Update the contributor checklist**

In `CONTRIBUTING.md`, find the provider checklist and add two items to it:

```markdown
- [ ] SDK errors are mapped to mamori classification sentinels with `%w`. See
      [Classifying errors](https://mamorigo.dev/docs/writing-a-provider#classifying-errors).
- [ ] `providertest.Config` supplies `Fail` and `Clear`, so the
      `ErrorClassification` conformance case runs rather than skips.
```

- [ ] **Step 4: Verify the site builds**

```bash
make site-build
```

Expected: build succeeds with no broken-link or frontmatter errors. If the site build is slow or npm is unavailable in the environment, at minimum confirm the markdown files parse by checking the dev server starts with `make site-dev`.

- [ ] **Step 5: Stage and report the commit message**

```bash
git add site/src/pages/docs/writing-a-provider.md site/src/pages/docs/concepts.md CONTRIBUTING.md
```

Suggested message:

```
docs: document error classification for providers and consumers

Adds the kind mapping table, the %w versus %v trap that silently breaks
errors.Is, and how to wire the Fail and Clear hooks so the conformance case
runs. Concepts gains a short consumer-facing entry, and the contributor
checklist now requires both.
```

---

## Self-Review

**Spec coverage.** This plan implements spec section 6 (workstream A) in full: 6.1 design in Task 1, 6.2 enforcement half 1 in Task 2, 6.3 mapping guidance in Task 4. The `x/otel` row of the spec's module table is Task 3.

Deliberately **not** in this plan, and where each lands:

| Spec item | Plan |
|---|---|
| 6.2 enforcement half 2 (31 SDK mapping tables), making `Fail` required, per-provider README tables | Plan 2 (A') |
| `docs/providers/*.md` error tables, README provider-table column | Plan 2 (A') |
| Sections 7 through 14 | Plans 3 through 7 |

**Placeholders.** None. Every code step carries complete code. The one instruction that defers to the reader is Task 3 Step 1's "reuse the existing metric-collection helper if present", which is a direction to avoid duplicating existing test scaffolding, not missing content; the full helper is supplied in case there is none.

**Type consistency.** `Kind` is used as `mamori.Kind` throughout. `ErrorKind` returns `Kind`; the otel task converts with `string(...)` before passing to `attribute.String`, which is required since `attribute.String` takes a `string` and `Kind` is a defined type. `SentinelFor` returns `error`. `Config.Fail` and `Config.Clear` signatures match between the field declaration in Task 2 Step 3, their use in the case body in Step 4, the self-test in Step 1, and the documentation example in Task 4.

**Known follow-up.** Task 2's `recordingTB` overrides a subset of `testing.TB` methods and relies on panics for `Fatal` and `Skip`. If the executor finds it fragile against the real `testing.TB` interface (which has unexported methods and so can only be embedded, not implemented), embedding `testing.TB` as written is the correct approach and the panics are caught by the enclosing `t.Run`. Should the panic-based control flow prove awkward, an acceptable alternative is to have `RunErrorClassification` return `error` and let `Run` call `tb.Fatal` on it; make that change in the implementation rather than working around it in the test.
