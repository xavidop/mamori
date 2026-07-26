---
layout: ../../layouts/DocsLayout.astro
title: Write a provider
---

# Write a provider

A provider is a small, self-contained Go module that resolves one URL scheme into values. The minimum is a type that implements `Scheme()` and `Resolve()` and registers itself; native watch and batching are optional add-ons. This page walks the implementation, the error-classification rules, and the conformance kit every provider must pass.

## Quick start

A working provider is a `Scheme()`, a `Resolve()`, and an `init` that calls `mamori.Register`. This is the whole shape:

```go
package myprovider

import (
	"context"

	"github.com/xavidop/mamori"
)

// Provider resolves refs of the "myscheme" scheme. It is safe for concurrent use.
type Provider struct {
	client backend // your backend client
}

// Option configures a Provider.
type Option func(*Provider)

// New builds a provider. With no options it uses a default client, so it is safe
// to Register from init before any credentials are present.
func New(opts ...Option) *Provider {
	p := &Provider{client: defaultClient()}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Scheme returns the single URL scheme this provider handles.
func (p *Provider) Scheme() string { return "myscheme" }

// Resolve fetches the current Value for ref.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	raw, err := p.client.get(ctx, ref.Path) // your backend call, honoring ctx
	if isNotFound(err) {
		return mamori.Value{}, mamori.ErrNotFound // MUST satisfy errors.Is
	}
	if err != nil {
		return mamori.Value{}, err
	}
	return mamori.Value{
		Bytes:     raw,
		Version:   mamori.VersionHash(raw), // or a native revision id
		Sensitive: true,                    // set on secret-bearing values
	}, nil
}

func init() { mamori.Register(New()) } // database/sql pattern; panics on a duplicate scheme
```

Once registered, a consumer refers to it from a struct tag with no extra wiring:

```go
type Config struct {
	APIKey string `source:"myscheme://backend/prd#STRIPE_API_KEY"`
}
```

## Set up the module

Each provider is its **own module** so a backend SDK never leaks into the core or into other providers. Create it under `providers/<name>`:

```text
providers/<name>/
  go.mod          module github.com/xavidop/mamori/providers/<name>
  <name>.go       the provider
  <name>_test.go  unit tests + providertest.Run against a fake
  README.md       scheme, ref grammar, auth, what is verified
```

The `go.mod` requires the core module and, during local development in the monorepo, points at it with a replace directive:

```text
module github.com/xavidop/mamori/providers/<name>

go 1.26

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
```

Consumers install it on its own, so your SDK dependency is opt-in:

```bash
go get github.com/xavidop/mamori/providers/<name>
```

## Implement Resolve

`Resolve` fetches the current `mamori.Value` for a `Ref`. A `Ref` gives you `Path` (the backend location), `Key` (the `#key` fragment, empty when absent), and `Opts` (query options). Fetch the payload, apply `#key` selection, and return the value with its metadata:

```go
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	raw, err := p.client.get(ctx, ref.Path)
	if isNotFound(err) {
		return mamori.Value{}, mamori.ErrNotFound
	}
	if err != nil {
		return mamori.Value{}, err
	}
	if ref.Key != "" { // #key selects from a JSON payload, identically everywhere
		raw, err = mamori.SelectKey(raw, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}
	return mamori.Value{
		Bytes:     raw,
		Version:   mamori.VersionHash(raw), // or a native backend revision
		Sensitive: true,                    // true for secret managers
	}, nil
}
```

These rules keep every provider interchangeable:

- Return an error satisfying `errors.Is(err, mamori.ErrNotFound)` for missing values (never a nil error with empty bytes).
- Set `Value.Version` from a native revision, or `mamori.VersionHash(bytes)`. It must change whenever the value changes, since that is what mamori uses for change detection.
- Use `mamori.SelectKey(payload, ref.Key)` for `#key` selection so it behaves the same across providers.
- Set `Sensitive: true` on secret-bearing values so redaction applies downstream. Never log the payload.
- Honor `ctx` in every network call.

## Map backend errors to kinds

`ErrNotFound` is the only error that changes mamori's behavior: it is what triggers a field's `default:` and `optional` handling. Every other failure should also be classified so telemetry (the `mamori.error.kind` attribute) and any caller using `mamori.ErrorKind` can tell an operator what actually went wrong instead of printing an opaque backend error.

Wrap the SDK error with the matching sentinel using **two `%w` verbs**, sentinel first:

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

Two `%w` verbs keep `errors.Is(err, mamori.ErrPermissionDenied)` matching the sentinel while `errors.As` can still reach the original SDK error type for anyone who wants it.

**Never use `%v` for the sentinel.** It flattens the sentinel into a plain string and destroys the chain:

```go
// WRONG: %v flattens the sentinel into a string and destroys the chain.
return mamori.Value{}, fmt.Errorf("secretsmanager: %v", mamori.ErrPermissionDenied)
```

Everything still compiles and the message still reads correctly, but `errors.Is` no longer matches and every failure reports as `unknown`. The `ErrorClassification` conformance case exists to catch exactly this.

### Which kind to use

Map each backend failure to the sentinel that names its cause. Leaving an error unmapped (reported as `unknown`) is fine; guessing is not, because a provider that reports `permission_denied` for a network timeout sends an operator down the wrong path.

| Kind | Use for |
|---|---|
| `ErrNotFound` | Key, secret, path, or version genuinely absent |
| `ErrPermissionDenied` | Authenticated but not authorized: IAM deny, Vault policy, RBAC |
| `ErrUnauthenticated` | Missing, malformed, or expired credentials; failed token renewal |
| `ErrUnavailable` | Network failure, DNS, timeout, 5xx, circuit open |
| `ErrRateLimited` | Throttling, quota exhaustion, 429 |
| `ErrInvalid` | The ref is malformed for this provider, or the payload cannot be parsed |
| (unmapped) | Anything else. Reports as `unknown`, which is an honest answer. |
| *(automatic)* | A `context.DeadlineExceeded` from `ctx` is classified as `unavailable` by `ErrorKind` itself; you do not need to map it. A plain `context.Canceled` still reports `unknown`, since the caller withdrew the request rather than the backend failing. |

## Add optional capabilities

Implement these only when the backend supports them. mamori detects each interface at runtime and uses it automatically (see [How it works](#how-it-works)).

### Watch for native changes

Implement `WatchableProvider` **only** for backends with native change notification (Vault leases, Kubernetes informers, Consul blocking queries, `fsnotify`). Providers without native watch are wrapped in a polling adapter by mamori, so never fake a `Watch` with an internal ticker.

```go
// Optional. The channel MUST close on ctx cancel, with no goroutine leaks.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		// ...subscribe; on each change emit mamori.Update{Value: v}...
		// ...on a transient error emit mamori.Update{Err: err} and keep the channel open...
	}()
	return ch, nil
}
```

### Resolve batches in one call

Implement `BatchProvider` when the backend can fetch many refs in one round trip (Secrets Manager `BatchGetSecretValue`, one file read for many keys). Key the result map by each input `Ref.Raw`, and omit not-found refs so mamori applies their defaults instead of failing the whole batch:

```go
// Optional. mamori calls this automatically when the interface is present.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	fetched, err := p.client.getMany(ctx, paths(refs))
	if err != nil {
		return nil, err
	}
	out := make(map[string]mamori.Value, len(refs))
	for _, ref := range refs {
		raw, ok := fetched[ref.Path]
		if !ok {
			continue // omit not-found refs; mamori applies the default
		}
		out[ref.Raw] = mamori.Value{Bytes: raw, Version: mamori.VersionHash(raw), Sensitive: true}
	}
	return out, nil
}
```

## Pass the conformance kit

`github.com/xavidop/mamori/providertest` runs one function that exercises resolution, not-found typing, error classification, `Version` monotonicity, concurrency, context cancellation, native watch, goroutine hygiene (goleak), and a no-payload-logging assertion. A provider that passes behaves identically to every other one.

Inject a client interface so the kit (and your unit tests) run against an in-memory fake, with live-backend tests behind a `//go:build integration` tag:

```go
func TestConformance(t *testing.T) {
	backend := newInMemoryFake()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return myprovider.New(myprovider.WithClient(backend)) },
		Ref:    func(key string) string { return "myscheme://" + key },
		Seed:   func(ctx context.Context, key, val string) error { return backend.set(key, val) },
		Mutate: func(ctx context.Context, key, val string) error { return backend.set(key, val) },
		Fail:   func(ctx context.Context, key string, err error) error { return backend.fail(key, err) },
		Clear:  func(ctx context.Context, key string) error { return backend.clear(key) },
	})
}
```

`Fail` and `Clear` are **required**: `Fail` makes the fake return a given error for a key, `Clear` cancels it. Together they power the `ErrorClassification` case (see [Map backend errors to kinds](#map-backend-errors-to-kinds)), which verifies a classified error survives your `Resolve` with its `errors.Is` chain intact. A provider that supplies neither `Fail` nor `NoResolveErrors: true` fails `providertest.Run` outright rather than being silently skipped.

If your backend genuinely has no per-key error surface (existence is a bool or a sentinel value, with nothing to inject), set `NoResolveErrors: true` instead, with a comment naming why. Live-backend integration tests (which cannot inject errors against a real backend) also set `NoResolveErrors: true`, since the unit-test conformance run already covers classification against the fake.

Build and test each module independently, with the workspace disabled (exactly what CI does per module):

```bash
cd providers/<name>
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

Or, from the repo root, `make test` / `make lint` run every module for you.

## Acceptance checklist

- [ ] `Scheme()` returns your scheme; `Resolve` returns `mamori.ErrNotFound` (via `errors.Is`) for missing values.
- [ ] Other failures are mapped to mamori's classification sentinels (`ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`) with `%w`. This is two things: `providertest.Config` supplies `Fail` and `Clear` (required, unless `NoResolveErrors: true` is set with a reason), AND a table test maps real SDK error values to kinds.
- [ ] `Value.Version` is set and changes when the value changes; secret-bearing values set `Sensitive: true`.
- [ ] `#key` uses `mamori.SelectKey`; the payload is never logged.
- [ ] `Watch` is implemented only for native-push backends, closes on `ctx` cancel, and leaks no goroutines.
- [ ] A client interface is injected so `providertest.Run` passes against an in-memory fake; live tests are behind `//go:build integration`.
- [ ] `go build`, `go vet`, and `go test` are clean with `GOWORK=off`; the README documents scheme, ref grammar, and auth.
- [ ] The module's `README.md` has an `## Error classification` section documenting what maps to what, mirrored onto the module's docs-site page.
- [ ] The module's row is flipped in both coverage tables: the root `README.md` and `site/src/pages/docs/providers/index.md`.

## How it works

**Why classification is conformance-enforced.** Two separate things have to be true, and each has its own guard. The `ErrorClassification` conformance case only proves your mapping is not destroyed in transit: it injects a mamori sentinel through your fake and checks it comes back out of `Resolve` with its `errors.Is` chain intact, which is exactly the `%v`-instead-of-`%w` bug. It does not prove your SDK mapping exists, because no in-memory fake can produce real SDK error values; that is what the table test over real SDK errors is for. This is why the checklist lists both, and why `Fail`/`Clear` are required rather than optional: a provider that supplied neither would silently skip the one guard that catches a broken chain. `KindUnknown` is a legal outcome throughout, on the principle that a provider that admits it does not know is better than one that guesses.

**Keeping the docs in step.** A provider's error classification lives in three places that drift apart if edited in isolation: the module `README.md`, its docs-site page, and the two coverage tables (root `README.md` and `site/src/pages/docs/providers/index.md`). The acceptance checklist requires updating all of them together so a reader never sees a provider claim one mapping in one place and another somewhere else.

**How mamori uses the optional interfaces.** mamori type-asserts each provider at runtime. A provider that implements `WatchableProvider` is watched natively; one that does not is wrapped in a polling adapter that calls `Resolve` on a schedule, which is why faking a `Watch` with an internal ticker is both unnecessary and wrong. A provider that implements `BatchProvider` has `ResolveBatch` called when several refs of its scheme resolve together; otherwise mamori falls back to individual `Resolve` calls. Because this coupling is by interface, adding a capability later never requires a consumer change. A provider that passes the kit earns a badge in the registry.
