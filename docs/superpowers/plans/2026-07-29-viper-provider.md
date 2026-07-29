# Viper Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `providers/viper` module so a `source:"viper://<key>"` ref reads whatever Viper resolved for that key, letting a team adopt mamori incrementally on top of an existing Viper setup.

**Architecture:** A conventional provider registering the `viper` scheme, resolving against a `*viper.Viper` (the global instance by default). Resolve only; mamori polls it. Viper's read API has no error return at all, so the provider takes `providertest`'s documented `NoResolveErrors` exemption rather than inventing a classification.

**Tech Stack:** Go 1.26, `github.com/spf13/viper` v1.21.0, `providertest` conformance kit.

**Spec:** `docs/superpowers/specs/2026-07-29-viper-provider-design.md`

## Global Constraints

- Go 1.26+. Test with `GOWORK=off` from inside `providers/viper`.
- **Run `golangci-lint run` in the module directory before reporting done.** CI lints every provider module with golangci-lint v2.12.2 and `.golangci.yml` sets `default: standard`, which includes `unused`. `go test`, `go vet`, and `gofmt` all pass on code the linter rejects.
- **Add the module to `.github/dependabot.yml`.** That file is hand-maintained while CI's test and lint matrices discover modules from disk, and a CI job cross-checks them. A new module without an entry fails the build. Verify with:
  `dirs=$(find . -name go.mod -not -path './site/*' -not -path './**/testdata/*' -exec dirname {} \; | sort | jq -R -s -c 'split("\n") | map(select(length > 0))') && python3 .github/scripts/check_dependabot_coverage.py "$dirs"`
- Also add the module to `go.work` at the repo root.
- Never use the em-dash character in any file, code comment, doc, or commit message.
- `Value.Sensitive = false`. Viper holds application configuration.
- A missing key returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.
- `Value.Version` is `mamori.VersionHash(data)`; Viper has no revision concept.
- Commits follow Conventional Commits, scope `feat(viper):`. No `BREAKING CHANGE:` footer.
- Do not run `git commit`; stage work and report it. The controller commits.

---

### Task 1: the `viper` provider

**Files:**
- Create: `providers/viper/go.mod`, `providers/viper/go.sum`
- Create: `providers/viper/viper.go`
- Create: `providers/viper/viper_test.go`
- Create: `providers/viper/README.md`
- Create: `site/src/pages/docs/providers/viper.md`
- Modify: `README.md` (root coverage table)
- Modify: `site/src/pages/docs/providers/index.md` (coverage table)
- Modify: `site/src/layouts/DocsLayout.astro` (sidebar entry)
- Modify: `skills/mamori/references/providers.md`
- Modify: `go.work`, `.github/dependabot.yml`

**Interfaces:**
- Produces: `Provider` struct; `New(opts ...Option) *Provider`; `WithViper(v *viper.Viper) Option`; `const Scheme = "viper"`.
- Consumes: `mamori.Provider`, `mamori.SelectKey`, `mamori.VersionHash`, `mamori.ErrNotFound`, `mamori.ErrInvalid`.

Read `providers/openfeature/openfeature.go` first: it is the most recently
reviewed provider in the tree and this one should look like it, minus the
error-classification machinery Viper does not need.

- [ ] **Step 1: Scaffold the module**

```bash
mkdir -p providers/viper
cd providers/viper
cat > go.mod <<'EOF'
module github.com/xavidop/mamori/providers/viper

go 1.26

require (
	github.com/spf13/viper v1.21.0
	github.com/xavidop/mamori v0.0.0
)

replace github.com/xavidop/mamori => ../..
EOF
GOWORK=off go mod tidy
```

Then add `./providers/viper` to `go.work`, matching how the others are listed.

- [ ] **Step 2: Write the failing tests**

Create `providers/viper/viper_test.go`. Note these drive a **real** `*viper.Viper`
rather than a fake: Viper is pure in-memory with no I/O once loaded, so the real
library is both simpler and a stronger test than a double that could drift from
Viper's actual precedence rules.

```go
package viper

import (
	"context"
	"errors"
	"testing"

	spf "github.com/spf13/viper"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func resolve(t *testing.T, p *Provider, raw string) (mamori.Value, error) {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return p.Resolve(context.Background(), ref)
}

func TestResolveRendersEachKind(t *testing.T) {
	v := spf.New()
	v.Set("s", "hello")
	v.Set("b", true)
	v.Set("i", 42)
	v.Set("f", 1.5)
	v.Set("table", map[string]any{"port": 5432})
	p := New(WithViper(v))

	tests := []struct{ name, ref, want string }{
		{"string", "viper://s", "hello"},
		{"bool", "viper://b", "true"},
		{"int", "viper://i", "42"},
		{"float", "viper://f", "1.5"},
		{"table", "viper://table", `{"port":5432}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(t, p, tt.ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if string(got.Bytes) != tt.want {
				t.Errorf("Bytes = %q, want %q", got.Bytes, tt.want)
			}
			if got.Sensitive {
				t.Error("Sensitive = true, want false: Viper holds application configuration")
			}
			if got.Version == "" {
				t.Error("Version is empty")
			}
		})
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithViper(spf.New()))
	if _, err := resolve(t, p, "viper://nope"); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

// TestResolveDefaultCountsAsSet pins a deliberate behaviour. Viper's IsSet
// reports true for a key whose only source is SetDefault, and this provider
// inherits that on purpose: a Viper default is a real configured value, and a
// team migrating incrementally will have keys that come only from one.
// Treating it as missing would silently substitute mamori's default for
// Viper's, which is a value change wearing the costume of a lookup.
func TestResolveDefaultCountsAsSet(t *testing.T) {
	v := spf.New()
	v.SetDefault("only.default", "from-default")
	got, err := resolve(t, New(WithViper(v)), "viper://only.default")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "from-default" {
		t.Errorf("Bytes = %q, want %q", got.Bytes, "from-default")
	}
}

// TestResolveRespectsPrecedence is the reason this provider exists. An
// explicit Set outranks a default in Viper, and the ref must return the winner
// Viper picked rather than any particular layer.
func TestResolveRespectsPrecedence(t *testing.T) {
	v := spf.New()
	v.SetDefault("server.port", 8080)
	v.Set("server.port", 9090)
	got, err := resolve(t, New(WithViper(v)), "viper://server.port")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "9090" {
		t.Errorf("Bytes = %q, want %q: the ref must return the value Viper resolved, not a single layer", got.Bytes, "9090")
	}
}

func TestResolveJSONPointerSelection(t *testing.T) {
	v := spf.New()
	v.Set("db", map[string]any{"creds": map[string]any{"port": 5432}})
	got, err := resolve(t, New(WithViper(v)), "viper://db#/creds/port")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "5432" {
		t.Errorf("Bytes = %q, want %q", got.Bytes, "5432")
	}
}

func TestResolveEmptyKey(t *testing.T) {
	_, err := resolve(t, New(WithViper(spf.New())), "viper://")
	if err == nil {
		t.Fatal("Resolve of an empty key returned nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
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
	v := spf.New()
	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return New(WithViper(v)) },
		Ref:        func(key string) string { return Scheme + "://" + key },
		PointerRef: func(key, frag string) string { return Scheme + "://" + key + frag },
		Seed:       func(_ context.Context, key, val string) error { v.Set(key, val); return nil },
		Mutate:     func(_ context.Context, key, val string) error { v.Set(key, val); return nil },
		// Viper's read API has no error return anywhere: Get(key) any and
		// IsSet(key) bool. There is no permission, rate-limit, unavailability,
		// or malformed-response case to inject, because the data is already in
		// memory by the time mamori asks. Any config-load failure happened
		// earlier, inside the application's own ReadInConfig call.
		NoResolveErrors: true,
	})
}
```

Note on the conformance `Seed`: the kit's `JSONPointerSelection` case seeds a
JSON document as a **string**. Check whether `SelectKey` receives that string
verbatim (it should, since a Go string is rendered through unchanged) and the
pointer resolves. If it does not, adjust `Seed` to store a decoded value for
pointer keys rather than weakening the assertion, and say in your report which
you did and why.

- [ ] **Step 3: Run and confirm failure**

```bash
cd providers/viper && GOWORK=off go test ./...
```

Expected: FAIL, undefined symbols.

- [ ] **Step 4: Implement the provider**

Create `providers/viper/viper.go`:

```go
// Package viper implements a mamori Provider backed by Viper
// (https://github.com/spf13/viper), the configuration library.
//
// It resolves refs of the form:
//
//	viper://<key>[#json-key]
//
// where <key> is a Viper key in its usual dotted form. The resolved Value is
// whatever Viper returns for that key, rendered as text:
//
//	Port     int    `source:"viper://server.port"`
//	LogLevel string `source:"viper://logging.level" default:"info"`
//
// The point of this provider is incremental adoption. Viper resolves a key by
// consulting explicit Set calls, then flags, then the environment, then the
// config file, then key/value stores, then defaults, and returns the winner. A
// viper:// ref returns that winner, so an application with an existing Viper
// setup can move fields into a typed, validated mamori struct one at a time
// without reimplementing Viper's precedence in struct tags, and can put secret
// material in a real secret store immediately.
//
// Values are never marked Sensitive: Viper holds application configuration.
// Move secret material to a secret manager rather than relying on this
// provider to treat Viper's contents as secret.
//
// Viper's read API has no error return (Get returns any, IsSet returns bool),
// so a missing key is the only failure this provider can report. There is
// nothing else to classify. A failure to load a config file happens earlier,
// inside the application's own ReadInConfig call.
//
// mamori polls this provider. A read here is an in-memory map lookup, so there
// is no cost for a native watch to avoid.
package viper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	spf "github.com/spf13/viper"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "viper"

func init() { mamori.Register(New()) }

// Provider resolves viper:// refs against a *viper.Viper.
//
// It is safe for concurrent use to the same degree the underlying Viper
// instance is: the provider adds no state of its own beyond the instance
// pointer, which is set at construction and never mutated.
type Provider struct {
	v *spf.Viper
}

// Compile-time interface check.
var _ mamori.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithViper resolves against an explicit Viper instance instead of the global
// one. Use it when the application keeps its own instance, and in tests.
func WithViper(v *spf.Viper) Option {
	return func(p *Provider) { p.v = v }
}

// New constructs a Viper provider. With no options it resolves against Viper's
// global instance, the one the package-level SetConfigFile, AutomaticEnv, and
// BindPFlag calls populate, which is what most Viper codebases use. An
// application that already calls viper.ReadInConfig gets working viper:// refs
// with no wiring.
//
// Construction performs no I/O and never fails, so registering from init is
// safe even before Viper has read anything.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}
	if p.v == nil {
		p.v = spf.GetViper()
	}
	return p
}

// Scheme returns "viper".
func (p *Provider) Scheme() string { return Scheme }

// Resolve reads the ref's key from Viper and renders the result as text.
//
// The key is passed to Viper verbatim and never split, so an instance
// configured with a non-default key delimiter works unchanged.
func (p *Provider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	key := ref.Path
	if key == "" {
		return mamori.Value{}, fmt.Errorf(
			"viper: ref %q must be viper://<key>[#json-key]: %w", ref.Raw, mamori.ErrInvalid)
	}

	// IsSet reports true for a key whose only source is SetDefault. That is
	// inherited deliberately: a Viper default is a real configured value, and
	// treating it as missing would substitute mamori's default for Viper's,
	// changing the value under the guise of a lookup.
	if !p.v.IsSet(key) {
		return mamori.Value{}, fmt.Errorf("viper: key %q is not set: %w", key, mamori.ErrNotFound)
	}

	data, err := render(p.v.Get(key))
	if err != nil {
		return mamori.Value{}, fmt.Errorf("viper: key %q: %w", key, err)
	}

	if ref.Key != "" {
		sel, serr := mamori.SelectKey(data, ref.Key)
		if serr != nil {
			return mamori.Value{}, serr
		}
		data = sel
	}

	return mamori.Value{
		Bytes:   data,
		Version: mamori.VersionHash(data), // Viper has no revision concept
		// Viper holds application configuration, not secrets.
		Sensitive: false,
	}, nil
}

// render turns a Viper value into the text form core decodes into a field.
//
// A string passes through unchanged rather than being JSON-encoded, so
// `viper://logging.level` yields info and not "info"; the quotes would survive
// into a string field and into any comparison against it. Scalars get their
// plain text form for the same reason. Everything else becomes JSON, which is
// also what a #json-key fragment selects against.
func render(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, fmt.Errorf("value is nil: %w", mamori.ErrNotFound)
	case string:
		return []byte(x), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case int:
		return []byte(strconv.Itoa(x)), nil
	case int32:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	case uint:
		return []byte(strconv.FormatUint(uint64(x), 10)), nil
	case uint64:
		return []byte(strconv.FormatUint(x, 10)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(x), 'g', -1, 32)), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'g', -1, 64)), nil
	case []byte:
		return x, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("value of type %T is not JSON-encodable: %w: %w", v, mamori.ErrInvalid, err)
	}
	return b, nil
}
```

- [ ] **Step 5: Run the tests**

```bash
cd providers/viper && GOWORK=off go test ./...
```

Expected: PASS. If `TestResolveRendersEachKind`'s float case fails on
formatting, check what Viper actually stores for the literal; adjust the
expected string to Go's canonical formatting rather than changing `render`
to produce something non-canonical.

- [ ] **Step 6: Verify each guard test has teeth**

Break the line each protects, confirm the named test fails, restore exactly.
Report the exact failure message from each:

- The `IsSet` check: make `Resolve` skip it, confirm `TestResolveNotFound` fails.
- The default-counts-as-set behaviour: replace `p.v.IsSet(key)` with a check that a key exists in `p.v.AllSettings()` (which excludes defaults), confirm `TestResolveDefaultCountsAsSet` fails.
- The `case string` branch in `render`: delete it so a string falls through to `json.Marshal`, confirm `TestResolveRendersEachKind/string` fails on the added quotes.
- `SelectKey` handling: skip the `ref.Key != ""` branch, confirm `TestResolveJSONPointerSelection` fails.

- [ ] **Step 7: Full verification**

```bash
cd providers/viper
GOWORK=off go mod tidy
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
golangci-lint run
```
then at the repo root: `go build ./...` and `go test ./...`.

`golangci-lint run` must report 0 issues.

- [ ] **Step 8: Register the module with CI**

Add the entry to `.github/dependabot.yml`, beside the other provider entries,
following their exact format (`package-ecosystem: gomod`, `directory`,
`schedule`, `labels` including `provider:viper`). Then verify:

```bash
cd <repo root>
dirs=$(find . -name go.mod -not -path './site/*' -not -path './**/testdata/*' -exec dirname {} \; | sort | jq -R -s -c 'split("\n") | map(select(length > 0))')
python3 .github/scripts/check_dependabot_coverage.py "$dirs"
```

Expected: "All N Go modules are covered by dependabot."

- [ ] **Step 9: Write the docs**

1. `providers/viper/README.md`: lead with the incremental-migration story, since that is the only reason to reach for this provider. Cover the ref grammar, which Viper instance is used and how to inject one, that Viper's precedence is what a ref returns, `#json-key` selection, the rendering rules, that a `SetDefault`-only key resolves rather than reporting not-found and why, and that values are not marked Sensitive with the advice to move secrets to a real secret store. Include a short section stating there is **no error classification** and why: Viper's read API has no error return, so not-found is the only failure available. Do not add an `## Error classification` table.
2. `site/src/pages/docs/providers/viper.md`: mirror it. Read a sibling page first and match its front matter and structure exactly.
3. Root `README.md`: add the coverage-table row.
4. `site/src/pages/docs/providers/index.md`: add the row. **The Errors column must be `n/a (no error surface)`**, matching the existing `unleash://` and `configcat://` rows, NOT a check mark. Consequently the prose sentence counting check-marked providers **does not change**: this provider adds no check mark. Verify by counting rows before and after.
5. `site/src/layouts/DocsLayout.astro`: add the sidebar entry.
6. `skills/mamori/references/providers.md`: add the row with an example. This provider is **not** secret-bearing, so it goes with the other config-style schemes, not in either secret list.

- [ ] **Step 10: Stage**

```bash
cd providers/viper && GOWORK=off go test ./... && cd ../.. && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.
