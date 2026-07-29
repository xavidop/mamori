# OpenFeature Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OpenFeature as an ordinary mamori provider module so a `source:"openfeature://<flag-key>"` ref resolves through whatever OpenFeature provider the application has installed.

**Architecture:** A new `providers/openfeature` module registering the `openfeature` scheme. It evaluates through `openfeature.IClient`, the SDK's own interface, against one fixed evaluation identity per process. It implements `Resolve` only; mamori polls it, exactly like the eight feature-flag providers already in the tree.

**Tech Stack:** Go 1.26, `github.com/open-feature/go-sdk` v1.17.2, `providertest` conformance kit.

**Spec:** `docs/superpowers/specs/2026-07-29-openfeature-provider-design.md`

## Global Constraints

- Go 1.26+. The module is tested with `GOWORK=off` so only its own `go.mod` is exercised.
- **Run `golangci-lint run` in the module directory before reporting done.** CI lints every provider module with golangci-lint v2.12.2 and `.golangci.yml` sets `default: standard`, which includes `unused`. `go test`, `go vet`, and `gofmt` all pass on code the linter rejects; an unused test-fake method is the usual trip.
- Never use the em-dash character in any file, code comment, doc, or commit message.
- `Value.Sensitive = false`. A feature flag is configuration, not a secret.
- Wrap SDK errors with mamori sentinels using `%w`, never `%v`, so `errors.Is` survives.
- A missing flag returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.
- `Value.Version` is always set: the evaluation's `Variant` when non-empty, `mamori.VersionHash(data)` otherwise.
- Passes `providertest.Run` with `Seed`, `Mutate`, `Fail`, `Clear`, and `PointerRef` all supplied.
- Docs ship with the feature: module `README.md`, a docs-site page, a row in **both** coverage tables (root `README.md` and `site/src/pages/docs/providers/index.md`), an `## Error classification` section in both README and site page, a sidebar entry in `site/src/layouts/DocsLayout.astro`, and the `skills/mamori/references/providers.md` table.
- One of the coverage tables carries a prose sentence counting check-marked providers. Count the rows; do not guess.
- Commits follow Conventional Commits, scope `feat(openfeature):`. No `BREAKING CHANGE:` footer.
- Do not run `git commit`; stage work and report it. The controller commits.

---

### Task 1: the `openfeature` provider

**Files:**
- Create: `providers/openfeature/go.mod`, `providers/openfeature/go.sum`
- Create: `providers/openfeature/openfeature.go`
- Create: `providers/openfeature/openfeature_test.go`
- Create: `providers/openfeature/README.md`
- Create: `site/src/pages/docs/providers/openfeature.md`
- Modify: `README.md` (root coverage table)
- Modify: `site/src/pages/docs/providers/index.md` (coverage table + count sentence)
- Modify: `site/src/layouts/DocsLayout.astro` (sidebar entry)
- Modify: `skills/mamori/references/providers.md`
- Modify: `go.work` (add the new module)

**Interfaces:**
- Produces: `Provider` struct; `New(opts ...Option) *Provider`; `WithClient`, `WithTargetingKey`, `WithAttributes`; `const Scheme = "openfeature"`.
- Consumes: `mamori.Provider`, `mamori.SelectKey`, `mamori.VersionHash`, and the error sentinels.

Read `providers/launchdarkly/launchdarkly.go` and `providers/growthbook/growthbook.go` first. This provider must look like them: same ref shape, same fixed-identity approach, same doc-comment structure.

- [ ] **Step 1: Scaffold the module**

```bash
mkdir -p providers/openfeature
cd providers/openfeature
cat > go.mod <<'EOF'
module github.com/xavidop/mamori/providers/openfeature

go 1.26

require (
	github.com/open-feature/go-sdk v1.17.2
	github.com/xavidop/mamori v0.0.0
)

replace github.com/xavidop/mamori => ../..
EOF
GOWORK=off go mod tidy
```

Then add the module to `go.work` at the repo root, matching how the other provider modules are listed there.

- [ ] **Step 2: Write the failing type-parsing test**

Create `providers/openfeature/openfeature_test.go`:

```go
package openfeature

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	of "github.com/open-feature/go-sdk/openfeature"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func TestParseFlagType(t *testing.T) {
	tests := []struct {
		raw     string
		want    flagType
		wantErr bool
	}{
		{raw: "openfeature://f", want: typeAuto},
		{raw: "openfeature://f?type=bool", want: typeBool},
		{raw: "openfeature://f?type=string", want: typeString},
		{raw: "openfeature://f?type=int", want: typeInt},
		{raw: "openfeature://f?type=float", want: typeFloat},
		{raw: "openfeature://f?type=object", want: typeObject},
		{raw: "openfeature://f?type=bogus", wantErr: true},
		{raw: "openfeature://f?type=", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			ref, err := mamori.ParseRef(tt.raw)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tt.raw, err)
			}
			got, err := parseFlagType(ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFlagType(%q) = %v, want error", tt.raw, got)
				}
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlagType(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseFlagType(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run it and confirm it fails**

```bash
cd providers/openfeature && GOWORK=off go test -run TestParseFlagType ./...
```

Expected: FAIL, `undefined: parseFlagType`.

- [ ] **Step 4: Write the fake client**

The fake implements only the five `*ValueDetails` methods the provider calls, and embeds `of.IClient` so the interface is satisfied without hand-writing the twenty methods this provider never touches. A call to any unimplemented method panics on the nil embedded interface, which is the correct outcome: it means the provider used an API this fake was never meant to model.

The fake also **counts calls per type**, because the fallback chain's stopping behaviour is a property this design depends on and a test cannot see it otherwise.

Append to `providers/openfeature/openfeature_test.go`:

```go
// ---------------------------------------------------------------------------
// In-memory fake for openfeature.IClient.
//
// of.IClient is embedded rather than fully implemented: this provider calls
// only the five *ValueDetails methods, and a nil embedded interface turns any
// other call into an immediate panic, which is what should happen if the
// provider starts using an API this fake does not model.
// ---------------------------------------------------------------------------

type fakeFlag struct {
	value   any
	variant string
	// kind constrains which evaluation succeeds, so the fake can model a
	// strictly-typed OpenFeature provider that reports TYPE_MISMATCH for the
	// wrong evaluation method. An empty kind accepts every method.
	kind string
}

type fakeClient struct {
	of.IClient // never populated; see the comment above

	mu    sync.Mutex
	flags map[string]fakeFlag
	fails map[string]of.ErrorCode
	// calls records how many times each evaluation method ran, keyed by type
	// name, so the fallback-chain tests can assert both order and stopping.
	calls map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		flags: map[string]fakeFlag{},
		fails: map[string]of.ErrorCode{},
		calls: map[string]int{},
	}
}

func (f *fakeClient) set(flag string, value any, variant, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[flag] = fakeFlag{value: value, variant: variant, kind: kind}
}

func (f *fakeClient) remove(flag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.flags, flag)
}

// fail makes the next evaluation of flag return code, until clear is called.
func (f *fakeClient) fail(flag string, code of.ErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[flag] = code
}

func (f *fakeClient) clear(flag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, flag)
}

func (f *fakeClient) callCount(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind]
}

// lookup is the shared body of all five evaluation methods. It returns the
// resolution detail the SDK would produce, plus the matching error.
func (f *fakeClient) lookup(kind, flag string) (any, of.ResolutionDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[kind]++

	if code, ok := f.fails[flag]; ok {
		return nil, of.ResolutionDetail{
			Reason:       of.ErrorReason,
			ErrorCode:    code,
			ErrorMessage: "injected " + string(code),
		}, fmt.Errorf("injected %s", code)
	}

	fl, ok := f.flags[flag]
	if !ok {
		return nil, of.ResolutionDetail{
			Reason:    of.ErrorReason,
			ErrorCode: of.FlagNotFoundCode,
		}, fmt.Errorf("flag %q not found", flag)
	}
	if fl.kind != "" && fl.kind != kind {
		return nil, of.ResolutionDetail{
			Reason:    of.ErrorReason,
			ErrorCode: of.TypeMismatchCode,
		}, fmt.Errorf("flag %q is not a %s", flag, kind)
	}
	return fl.value, of.ResolutionDetail{
		Variant: fl.variant,
		Reason:  of.StaticReason,
	}, nil
}

func (f *fakeClient) BooleanValueDetails(_ context.Context, flag string, def bool, _ of.EvaluationContext, _ ...of.Option) (of.BooleanEvaluationDetails, error) {
	v, rd, err := f.lookup("bool", flag)
	out := of.BooleanEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	b, ok := v.(bool)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a bool", flag)
	}
	out.Value = b
	return out, nil
}

func (f *fakeClient) StringValueDetails(_ context.Context, flag string, def string, _ of.EvaluationContext, _ ...of.Option) (of.StringEvaluationDetails, error) {
	v, rd, err := f.lookup("string", flag)
	out := of.StringEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	s, ok := v.(string)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a string", flag)
	}
	out.Value = s
	return out, nil
}

func (f *fakeClient) IntValueDetails(_ context.Context, flag string, def int64, _ of.EvaluationContext, _ ...of.Option) (of.IntEvaluationDetails, error) {
	v, rd, err := f.lookup("int", flag)
	out := of.IntEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	n, ok := v.(int64)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not an int", flag)
	}
	out.Value = n
	return out, nil
}

func (f *fakeClient) FloatValueDetails(_ context.Context, flag string, def float64, _ of.EvaluationContext, _ ...of.Option) (of.FloatEvaluationDetails, error) {
	v, rd, err := f.lookup("float", flag)
	out := of.FloatEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	x, ok := v.(float64)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a float", flag)
	}
	out.Value = x
	return out, nil
}

func (f *fakeClient) ObjectValueDetails(_ context.Context, flag string, def any, _ of.EvaluationContext, _ ...of.Option) (of.InterfaceEvaluationDetails, error) {
	v, rd, err := f.lookup("object", flag)
	out := of.InterfaceEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	out.Value = v
	return out, nil
}
```

- [ ] **Step 5: Write the failing behaviour tests**

```go
func newTestProvider(c of.IClient) *Provider { return New(WithClient(c)) }

func resolve(t *testing.T, p *Provider, raw string) (mamori.Value, error) {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return p.Resolve(context.Background(), ref)
}

func TestResolveRendersEachType(t *testing.T) {
	tests := []struct {
		name  string
		ref   string
		value any
		kind  string
		want  string
	}{
		{name: "bool", ref: "openfeature://f?type=bool", value: true, kind: "bool", want: "true"},
		{name: "string", ref: "openfeature://f?type=string", value: "hello", kind: "string", want: "hello"},
		{name: "int", ref: "openfeature://f?type=int", value: int64(42), kind: "int", want: "42"},
		{name: "float", ref: "openfeature://f?type=float", value: 1.5, kind: "float", want: "1.5"},
		{name: "object", ref: "openfeature://f?type=object", value: map[string]any{"a": float64(1)}, kind: "object", want: `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeClient()
			fake.set("f", tt.value, "", tt.kind)
			v, err := resolve(t, newTestProvider(fake), tt.ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := string(v.Bytes); got != tt.want {
				t.Errorf("Bytes = %q, want %q", got, tt.want)
			}
			if v.Sensitive {
				t.Error("Sensitive = true, want false: a feature flag is configuration, not a secret")
			}
		})
	}
}

// TestResolveAutoFallbackOrderAndStop asserts the untyped chain tries object,
// then bool, then string, AND stops at the first success. The stopping half
// matters as much as the order: a chain that kept going would turn one
// resolve into three evaluations against a vendor that may rate-limit or bill
// per call.
func TestResolveAutoFallbackOrderAndStop(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", true, "", "bool") // only a bool evaluation succeeds
	v, err := resolve(t, newTestProvider(fake), "openfeature://f")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "true" {
		t.Errorf("Bytes = %q, want %q", got, "true")
	}
	if n := fake.callCount("object"); n != 1 {
		t.Errorf("object evaluations = %d, want 1: object must be tried first", n)
	}
	if n := fake.callCount("bool"); n != 1 {
		t.Errorf("bool evaluations = %d, want 1", n)
	}
	if n := fake.callCount("string"); n != 0 {
		t.Errorf("string evaluations = %d, want 0: the chain must stop at the first success", n)
	}
}

// TestResolveTypedPinSkipsFallback is the property the docs tell operators to
// rely on when they pin ?type=.
func TestResolveTypedPinSkipsFallback(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", true, "", "bool")
	if _, err := resolve(t, newTestProvider(fake), "openfeature://f?type=bool"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n := fake.callCount("object"); n != 0 {
		t.Errorf("object evaluations = %d, want 0: ?type= must pin the evaluation", n)
	}
	if n := fake.callCount("bool"); n != 1 {
		t.Errorf("bool evaluations = %d, want 1", n)
	}
}

// TestResolveAutoExhaustedReportsInvalid covers a flag no evaluation in the
// chain can read. It must not report not-found, which would make mamori apply
// a default for a flag that exists.
func TestResolveAutoExhaustedReportsInvalid(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", 1.5, "", "float") // float is not in the auto chain
	_, err := resolve(t, newTestProvider(fake), "openfeature://f")
	if err == nil {
		t.Fatal("Resolve returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("exhausted fallback reported ErrNotFound; the flag exists, so a default must not be applied")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "type=") {
		t.Errorf("error %q does not tell the user to pin ?type=", err)
	}
}

func TestResolveVersionPrefersVariant(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "on", "variant-a", "string")
	v, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != "variant-a" {
		t.Errorf("Version = %q, want the evaluation's variant %q", v.Version, "variant-a")
	}
}

func TestResolveVersionFallsBackToHash(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "on", "", "string") // no variant
	v, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := mamori.VersionHash(v.Bytes); v.Version != want {
		t.Errorf("Version = %q, want the VersionHash fallback %q", v.Version, want)
	}
}

func TestResolveJSONPointerSelection(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", map[string]any{"db": map[string]any{"port": float64(5432)}}, "", "object")
	v, err := resolve(t, newTestProvider(fake), "openfeature://f#/db/port?type=object")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "5432" {
		t.Errorf("Bytes = %q, want %q", got, "5432")
	}
}

func TestResolveNotFound(t *testing.T) {
	fake := newFakeClient()
	_, err := resolve(t, newTestProvider(fake), "openfeature://missing?type=string")
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		code of.ErrorCode
		want mamori.Kind
	}{
		{of.FlagNotFoundCode, mamori.KindNotFound},
		{of.TypeMismatchCode, mamori.KindInvalid},
		{of.ParseErrorCode, mamori.KindInvalid},
		{of.InvalidContextCode, mamori.KindInvalid},
		{of.TargetingKeyMissingCode, mamori.KindInvalid},
		{of.ProviderNotReadyCode, mamori.KindUnavailable},
		{of.ProviderFatalCode, mamori.KindUnavailable},
		{of.GeneralCode, mamori.KindUnknown},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			fake := newFakeClient()
			fake.set("f", "v", "", "string")
			fake.fail("f", tt.code)
			_, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
			if err == nil {
				t.Fatal("Resolve returned nil error")
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Errorf("ErrorKind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTargetingKeyAndAttributes(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "v", "", "string")
	p := New(WithClient(fake), WithTargetingKey("svc-a"), WithAttributes(map[string]any{"region": "eu"}))
	if p.evalCtx.TargetingKey() != "svc-a" {
		t.Errorf("TargetingKey = %q, want %q", p.evalCtx.TargetingKey(), "svc-a")
	}
	if got := p.evalCtx.Attributes()["region"]; got != "eu" {
		t.Errorf("attribute region = %v, want %q", got, "eu")
	}
}

func TestDefaultTargetingKey(t *testing.T) {
	p := New(WithClient(newFakeClient()))
	if p.evalCtx.TargetingKey() != defaultTargetingKey {
		t.Errorf("TargetingKey = %q, want %q", p.evalCtx.TargetingKey(), defaultTargetingKey)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != Scheme {
		t.Errorf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestRegisteredScheme(t *testing.T) {
	for _, s := range mamori.RegisteredSchemes() {
		if s == Scheme {
			return
		}
	}
	t.Errorf("scheme %q was not registered by init()", Scheme)
}

func TestConformance(t *testing.T) {
	fake := newFakeClient()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		// The conformance kit seeds string values, so pin the evaluation to
		// string rather than paying the auto chain's extra calls on every case.
		Ref:        func(key string) string { return Scheme + "://" + key + "?type=string" },
		PointerRef: func(key, frag string) string { return Scheme + "://" + key + frag + "?type=object" },
		Seed:       func(_ context.Context, key, val string) error { fake.set(key, val, "", "string"); return nil },
		Mutate:     func(_ context.Context, key, val string) error { fake.set(key, val, "", "string"); return nil },
		Fail:       func(_ context.Context, key string, _ error) error { fake.fail(key, of.GeneralCode); return nil },
		Clear:      func(_ context.Context, key string) error { fake.clear(key); return nil },
	})
}
```

Note on `PointerRef`: the kit seeds a JSON document as a **string**, and this
ref pins `?type=object`. If the fake's string-kind seed makes the object
evaluation report `TYPE_MISMATCH`, adjust `Seed` so a pointer key is stored
with an empty `kind` (accepting any evaluation) rather than weakening the
assertion. Say in your report which you did and why.

- [ ] **Step 6: Run and confirm failure**

```bash
cd providers/openfeature && GOWORK=off go test ./...
```

Expected: FAIL, undefined symbols.

- [ ] **Step 7: Implement the provider**

Create `providers/openfeature/openfeature.go`:

```go
// Package openfeature implements a mamori Provider backed by OpenFeature
// (https://openfeature.dev), the vendor-neutral feature-flag standard.
//
// It resolves refs of the form:
//
//	openfeature://<flag-key>[#json-key][?type=bool|string|int|float|object]
//
// where <flag-key> is a flag in whichever OpenFeature provider the application
// has installed with openfeature.SetProvider. The resolved Value is the
// evaluated flag rendered as text:
//
//	NewCheckout bool   `source:"openfeature://new-checkout?type=bool"`
//	Limits      string `source:"openfeature://limits?type=object"`
//	MaxUpload   int    `source:"openfeature://limits#/upload/maxMB?type=object"`
//
// Evaluation identity is fixed for the process. Every ref is evaluated against
// one evaluation context, whose targeting key defaults to "mamori" and is
// overridable with WithTargetingKey, plus any static attributes given to
// WithAttributes. This provider therefore does NOT do per-user targeting: a
// mamori field holds one value for the whole process, not one value per
// request. Targeting that varies per end user belongs in the application's own
// OpenFeature calls, not in a config field.
//
// Values are never marked Sensitive: a feature flag is configuration, not a
// secret. OpenFeature has no native change notification exposed here, so this
// provider does not implement WatchableProvider and mamori polls it.
package openfeature

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	of "github.com/open-feature/go-sdk/openfeature"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "openfeature"

// defaultTargetingKey is the evaluation context's targeting key when none is
// given. It matches the "mamori" default the launchdarkly provider uses for
// the same purpose.
const defaultTargetingKey = "mamori"

// clientName is the name given to the SDK client this provider creates when no
// client is injected.
const clientName = "mamori"

func init() { mamori.Register(New()) }

// flagType is which OpenFeature evaluation method a ref selects.
type flagType int

const (
	// typeAuto tries object, then bool, then string. See resolveAuto.
	typeAuto flagType = iota
	typeBool
	typeString
	typeInt
	typeFloat
	typeObject
)

func (t flagType) String() string {
	switch t {
	case typeBool:
		return "bool"
	case typeString:
		return "string"
	case typeInt:
		return "int"
	case typeFloat:
		return "float"
	case typeObject:
		return "object"
	}
	return "auto"
}

// Provider resolves openfeature:// refs by evaluating flags through an
// OpenFeature client. It is safe for concurrent use: the client is set at
// construction and never mutated.
type Provider struct {
	client  of.IClient
	evalCtx of.EvaluationContext
}

// Compile-time interface check.
var _ mamori.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	client       of.IClient
	targetingKey string
	attributes   map[string]any
}

// WithClient injects the OpenFeature client to evaluate through, instead of
// the default one this provider creates. Use it to supply an application's own
// named client, or an in-memory fake in tests.
func WithClient(c of.IClient) Option {
	return func(cfg *providerConfig) { cfg.client = c }
}

// WithTargetingKey sets the evaluation context's targeting key, which
// identifies the subject every flag is evaluated for. It defaults to "mamori".
//
// This is a per-process identity, not a per-request one. See the package doc.
func WithTargetingKey(k string) Option {
	return func(cfg *providerConfig) { cfg.targetingKey = k }
}

// WithAttributes adds static evaluation-context attributes, so a deployment can
// be targeted by region, tier, or any other dimension that is constant for the
// process.
func WithAttributes(m map[string]any) Option {
	return func(cfg *providerConfig) { cfg.attributes = m }
}

// New constructs an OpenFeature provider. With no options it evaluates through
// openfeature.NewClient("mamori"), which resolves against whatever provider the
// application installed with openfeature.SetProvider.
//
// Construction performs no I/O and never fails, so registering it from init is
// safe even before an OpenFeature provider has been set. An evaluation made
// before one is ready reports PROVIDER_NOT_READY, which maps to
// mamori.ErrUnavailable and is retried like any other transient failure.
func New(opts ...Option) *Provider {
	cfg := providerConfig{targetingKey: defaultTargetingKey}
	for _, opt := range opts {
		opt(&cfg)
	}
	p := &Provider{client: cfg.client}
	if p.client == nil {
		p.client = of.NewClient(clientName)
	}
	p.evalCtx = of.NewEvaluationContext(cfg.targetingKey, cfg.attributes)
	return p
}

// Scheme returns "openfeature".
func (p *Provider) Scheme() string { return Scheme }

// parseFlagType reads the optional ?type= option. An unrecognized or empty
// value is rejected rather than silently falling back to the auto chain: a
// typo like ?type=boolean would otherwise resolve through a different code
// path than the author intended and only show up as a wrong value.
// Ref.Opts is a url.Values, so an absent option and one present but empty are
// distinguishable: ?type= with no value is a mistake worth reporting, not a
// request for the auto chain.
func parseFlagType(ref mamori.Ref) (flagType, error) {
	if _, ok := ref.Opts["type"]; !ok {
		return typeAuto, nil
	}
	switch strings.TrimSpace(ref.Opt("type")) {
	case "bool":
		return typeBool, nil
	case "string":
		return typeString, nil
	case "int":
		return typeInt, nil
	case "float":
		return typeFloat, nil
	case "object":
		return typeObject, nil
	}
	return typeAuto, fmt.Errorf(
		"openfeature: ref %q has unsupported ?type=%q; want bool, string, int, float, or object: %w",
		ref.Raw, ref.Opt("type"), mamori.ErrInvalid)
}

// Resolve evaluates the ref's flag and renders the result as text.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if ref.Path == "" {
		return mamori.Value{}, fmt.Errorf(
			"openfeature: ref %q must be openfeature://<flag-key>[#json-key][?type=t]: %w",
			ref.Raw, mamori.ErrInvalid)
	}
	typ, err := parseFlagType(ref)
	if err != nil {
		return mamori.Value{}, err
	}

	data, variant, err := p.evaluate(ctx, ref, typ)
	if err != nil {
		return mamori.Value{}, err
	}

	if ref.Key != "" {
		sel, serr := mamori.SelectKey(data, ref.Key)
		if serr != nil {
			return mamori.Value{}, serr
		}
		data = sel
	}

	version := variant
	if version == "" {
		version = mamori.VersionHash(data)
	}
	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a feature flag is configuration, not a secret
	}, nil
}

// evaluate dispatches to the pinned evaluation, or runs the auto chain.
func (p *Provider) evaluate(ctx context.Context, ref mamori.Ref, typ flagType) ([]byte, string, error) {
	if typ == typeAuto {
		return p.evaluateAuto(ctx, ref)
	}
	return p.evaluateOne(ctx, ref, typ)
}

// autoChain is the order the untyped fallback tries. object comes first
// because it is the only one that can carry a structured payload, and a
// provider that answers it can answer for any flag shape; bool is next because
// it is the most common flag type by far.
var autoChain = []flagType{typeObject, typeBool, typeString}

// evaluateAuto tries each type in autoChain, stopping at the first that does
// not report TYPE_MISMATCH.
//
// Stopping matters as much as ordering: every attempt is a real evaluation
// against the configured vendor, which may rate-limit or bill per call, so an
// author who knows the type should pin ?type= and pay exactly one.
func (p *Provider) evaluateAuto(ctx context.Context, ref mamori.Ref) ([]byte, string, error) {
	var lastErr error
	for _, typ := range autoChain {
		data, variant, err := p.evaluateOne(ctx, ref, typ)
		if err == nil {
			return data, variant, nil
		}
		lastErr = err
		// Only a type mismatch is worth retrying with another shape. A missing
		// flag, an unready provider, or a bad context would give the same
		// answer for every type, so retrying would just multiply the failure.
		if !mamoriKindIs(err, mamori.KindInvalid) || !strings.Contains(err.Error(), string(of.TypeMismatchCode)) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf(
		"openfeature: flag %q could not be evaluated as object, bool, or string; pin the flag's type with ?type= (bool, string, int, float, object): %w",
		ref.Path, errOrInvalid(lastErr))
}

// mamoriKindIs reports whether err classifies as kind.
func mamoriKindIs(err error, kind mamori.Kind) bool { return mamori.ErrorKind(err) == kind }

// errOrInvalid returns err when it already carries a mamori sentinel, and
// ErrInvalid otherwise, so the exhausted-chain error is always classifiable.
func errOrInvalid(err error) error {
	if err != nil && mamori.ErrorKind(err) != mamori.KindUnknown {
		return err
	}
	return mamori.ErrInvalid
}

// evaluateOne performs a single typed evaluation and renders the result.
func (p *Provider) evaluateOne(ctx context.Context, ref mamori.Ref, typ flagType) ([]byte, string, error) {
	flag := ref.Path
	switch typ {
	case typeBool:
		d, err := p.client.BooleanValueDetails(ctx, flag, false, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatBool(d.Value)), d.Variant, nil
	case typeString:
		d, err := p.client.StringValueDetails(ctx, flag, "", p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(d.Value), d.Variant, nil
	case typeInt:
		d, err := p.client.IntValueDetails(ctx, flag, 0, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatInt(d.Value, 10)), d.Variant, nil
	case typeFloat:
		d, err := p.client.FloatValueDetails(ctx, flag, 0, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatFloat(d.Value, 'g', -1, 64)), d.Variant, nil
	case typeObject:
		d, err := p.client.ObjectValueDetails(ctx, flag, nil, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		b, jerr := json.Marshal(d.Value)
		if jerr != nil {
			return nil, "", fmt.Errorf(
				"openfeature: flag %q evaluated to a value that is not JSON-encodable: %w: %w",
				flag, mamori.ErrInvalid, jerr)
		}
		return b, d.Variant, nil
	}
	return nil, "", fmt.Errorf("openfeature: unhandled flag type %v: %w", typ, mamori.ErrInvalid)
}

// wrap annotates an evaluation failure with the ref and classifies it from the
// resolution detail's ErrorCode.
//
// The code is read from the ResolutionDetail rather than the error, because
// openfeature.ResolutionError keeps its code unexported and offers no
// accessor; the detail struct is the only place the code is readable.
func (p *Provider) wrap(flag string, typ flagType, rd of.ResolutionDetail, err error) error {
	sentinel := classifyOF(rd.ErrorCode)
	if sentinel == nil {
		return fmt.Errorf("openfeature: evaluate %q as %s: %w", flag, typ, err)
	}
	return fmt.Errorf("openfeature: evaluate %q as %s [%s]: %w: %w",
		flag, typ, rd.ErrorCode, sentinel, err)
}

// classifyOF maps an OpenFeature error code onto a mamori classification
// sentinel, returning nil for a code mamori does not classify.
//
// GeneralCode is deliberately unmapped. It is OpenFeature's catch-all and says
// nothing about whether the cause is transient, a permission problem, or a
// bug, so guessing would send an operator down the wrong path. This mirrors
// how the aws provider leaves DecryptionFailure unclassified for the same
// reason.
func classifyOF(code of.ErrorCode) error {
	switch code {
	case of.FlagNotFoundCode:
		return mamori.ErrNotFound
	case of.TypeMismatchCode, of.ParseErrorCode, of.InvalidContextCode, of.TargetingKeyMissingCode:
		return mamori.ErrInvalid
	case of.ProviderNotReadyCode, of.ProviderFatalCode:
		return mamori.ErrUnavailable
	}
	return nil
}
```

These APIs were all verified against the real packages before this plan was
written, so they should compile as given:

- `mamori.Ref.Opts` is a `url.Values`, and `Ref.Opt(name) string` is the
  accessor used elsewhere in the tree.
- `mamori.RegisteredSchemes() []string` exists, for the registration test.
- `of.EvaluationContext` has `TargetingKey() string` and
  `Attributes() map[string]any`, which the option tests use.
- `of.GenericEvaluationDetails[T]` embeds `EvaluationDetails`, which embeds
  `ResolutionDetail`, so `d.Variant` and `d.ResolutionDetail` both resolve
  through promotion.

If any of them does not compile, fix the code and say so in your report rather
than working around it.

- [ ] **Step 8: Run the tests**

```bash
cd providers/openfeature && GOWORK=off go test ./...
```

Expected: PASS. Fix any compile mismatch against the real SDK rather than
adjusting the tests to match a broken implementation.

- [ ] **Step 9: Verify each guard test has teeth**

For each of these, break the line it guards, confirm the named test fails,
then restore it exactly:

- `autoChain` order: reverse it, confirm `TestResolveAutoFallbackOrderAndStop` fails.
- The `err == nil` early return in `evaluateAuto`: remove it so the loop always runs to the end, confirm the same test's "must stop at the first success" assertion fails.
- The `typ == typeAuto` branch in `evaluate`: always run the auto chain, confirm `TestResolveTypedPinSkipsFallback` fails.
- The `variant` preference in `Resolve`: always use `VersionHash`, confirm `TestResolveVersionPrefersVariant` fails.
- `classifyOF`'s `FlagNotFoundCode` case: remove it, confirm `TestResolveNotFound` and `TestErrorClassification` fail.

Report the exact failure message each mutation produced.

- [ ] **Step 10: Full verification**

```bash
cd providers/openfeature
GOWORK=off go mod tidy
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
golangci-lint run
```
then `go build ./...` at the repo root.

`golangci-lint run` must report 0 issues.

- [ ] **Step 11: Write the docs**

1. `providers/openfeature/README.md`: schemes, ref grammar, `?type=` including the recommendation to pin it in production and why (one evaluation instead of up to three), `#json-key` selection, auth/setup (the application installs its own OpenFeature provider; this one evaluates through it), the fixed per-process evaluation identity and the explicit statement that this is **not** per-user targeting, that values are not marked Sensitive, and an `## Error classification` section covering all eight codes with `GENERAL` documented as deliberately unmapped.
2. `site/src/pages/docs/providers/openfeature.md`: mirror it. Read a sibling flag-provider page such as `site/src/pages/docs/providers/launchdarkly.md` first and match its front matter and structure exactly.
3. Root `README.md`: add the row to the coverage table, and the `go get` line if that section lists modules individually.
4. `site/src/pages/docs/providers/index.md`: add the row, and update the prose sentence that counts check-marked providers. Count the rows.
5. `site/src/layouts/DocsLayout.astro`: add the sidebar entry.
6. `skills/mamori/references/providers.md`: add the row with an example. This provider is **not** secret-bearing, so it must not go in either secret list; add it where the other config-style schemes are named.

- [ ] **Step 12: Stage**

```bash
cd providers/openfeature && GOWORK=off go test ./... && cd ../.. && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.
