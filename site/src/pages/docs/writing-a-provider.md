---
layout: ../../layouts/DocsLayout.astro
title: Write a provider
---

# Write a provider

A provider is a small, self-contained Go module that resolves one URL scheme. This page is the complete contract: implement the interface, follow the rules, and pass the conformance kit.

## Module layout

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

## The interface

```go
package myprovider

import (
	"context"
	"github.com/xavidop/mamori"
)

type Provider struct{ /* client, config */ }

func New(opts ...Option) *Provider { /* ... */ return &Provider{} }

func (p *Provider) Scheme() string { return "myscheme" }

func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	raw, err := p.fetch(ctx, ref.Path) // your backend call
	if isNotFound(err) {
		return mamori.Value{}, mamori.ErrNotFound // MUST satisfy errors.Is
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
		Version:   backendRevision, // or mamori.VersionHash(raw)
		Sensitive: true,            // true for secret managers
	}, nil
}

func init() { mamori.Register(New()) } // database/sql pattern; panics on duplicate scheme
```

## Rules

These keep every provider interchangeable:

- Return an error satisfying `errors.Is(err, mamori.ErrNotFound)` for missing values (never nil error + empty bytes).
- Set `Value.Version` from a native revision, or `mamori.VersionHash(bytes)`. It must change when the value changes.
- Use `mamori.SelectKey(payload, ref.Key)` for `#key` selection so it behaves the same across providers.
- Never log the payload.
- Implement `Watch` **only** if the backend has native change notification; otherwise mamori polls for you. Implement `ResolveBatch` if the backend can fetch many refs in one call.
- Honor `ctx` in every network call.

## Classifying errors

`ErrNotFound` tells mamori a value is absent, which is what triggers `default:`
and `optional` handling. Every other failure should also be classified, so that
telemetry (the `mamori.error.kind` attribute) and any code calling `mamori.ErrorKind` can tell an operator what is actually wrong
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

Use `%w` for both the sentinel and the SDK error, in that order. Go's
`fmt.Errorf` supports more than one `%w` verb, and wrapping both keeps
`errors.Is(err, mamori.ErrPermissionDenied)` working for mamori while also
leaving `errors.As` able to reach the original SDK error type for anyone who
wants it. Formatting the SDK error with `%v` instead only wraps the sentinel;
the SDK error becomes a flattened string and `errors.As` can no longer reach
it.

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
| *(automatic)* | A `context.DeadlineExceeded` from `ctx` is classified as `unavailable` by `ErrorKind` itself; you do not need to map it. A plain `context.Canceled` still reports `unknown`, since the caller withdrew the request rather than the backend failing. |

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

`Fail` and `Clear` on your `providertest.Config` are REQUIRED. They make your
fake backend return a given error for a key, and stop:

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

If your provider's backend genuinely has no per-key error surface (existence
is a bool or a sentinel value, with nothing to inject), set
`NoResolveErrors: true` instead, with a comment naming why. This is the only
exemption from supplying `Fail`/`Clear`: a provider that supplies neither is a
hard error, not a silent skip.

## Native watch

```go
// Optional. Implement only for backends that can push (informers, blocking queries, fsnotify).
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch) // MUST close on ctx cancel; no goroutine leaks
		// ...subscribe, emit mamori.Update{Value: v} on change...
	}()
	return ch, nil
}
```

## The conformance kit

`github.com/xavidop/mamori/providertest` runs one function that exercises resolution, not-found typing, `Version` monotonicity, concurrency, context cancellation, native watch, goroutine hygiene (goleak), and a no-payload-logging assertion. A provider that passes behaves identically to every other one.

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

Inject a client interface so the kit (and your unit tests) run against an in-memory fake, with live-backend tests behind a `//go:build integration` tag. A provider that passes the kit earns a badge in the registry.

`Fail` and `Clear` (shown above) let the fake inject a classified error for a key and then cancel it, and are required so the `ErrorClassification` case (see [Classifying errors](#classifying-errors)) runs. A provider that supplies neither `Fail` nor `NoResolveErrors: true` fails `providertest.Run` outright; live-backend integration tests (which cannot inject errors against a real backend) set `NoResolveErrors: true` instead, since the unit-test conformance run already covers classification against the fake.

## Build and test

Each module is built and tested independently, with the workspace disabled (this is exactly what CI does per module):

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
- [ ] Other failures are mapped to mamori's classification sentinels (`ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`) with `%w`. This is two separate requirements: `providertest.Config` supplies `Fail` and `Clear` (required, unless `NoResolveErrors: true` is set with a reason) so `ErrorClassification` passes (which only proves your mapping is not destroyed in transit), AND a table test maps real SDK error values to kinds (which is what actually proves the mapping exists).
- [ ] `Value.Version` is set and changes when the value changes; secret-bearing values set `Sensitive: true`.
- [ ] `#key` uses `mamori.SelectKey`; the payload is never logged.
- [ ] `Watch` is implemented only for native-push backends, closes on `ctx` cancel, and leaks no goroutines.
- [ ] A client interface is injected so `providertest.Run` passes against an in-memory fake; live tests are behind `//go:build integration`.
- [ ] `go build`, `go vet`, and `go test` are clean with `GOWORK=off`; the README documents scheme, ref grammar, and auth.
- [ ] The module's `README.md` has an `## Error classification` section documenting what maps to what.
- [ ] That section is mirrored onto the module's docs-site page.
- [ ] The module's row is flipped in both coverage tables: the root `README.md` and `site/src/pages/docs/providers/index.md`.
