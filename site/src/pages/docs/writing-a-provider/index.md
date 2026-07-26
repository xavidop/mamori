---
layout: ../../../layouts/DocsLayout.astro
title: Write a provider
---

# Write a provider

A provider is a self-contained Go module that resolves one URL scheme into values. The minimum interface is a type with `Scheme()` and `Resolve()` that registers itself; native watch and batching are optional.

```go
type Provider interface {
	Scheme() string
	Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)
}
```

## Quick start

A provider is a `Scheme()`, a `Resolve()`, and an `init` that calls `mamori.Register`:

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

func init() { mamori.Register(New()) } // panics on a duplicate scheme
```

A consumer refers to it from a struct tag, no extra wiring:

```go
type Config struct {
	APIKey string `source:"myscheme://backend/prd#STRIPE_API_KEY"`
}
```

## Set up the module

Each provider is its **own module** so its backend SDK never leaks into the core or other providers. Create it under `providers/<name>`:

```text
providers/<name>/
  go.mod          module github.com/xavidop/mamori/providers/<name>
  <name>.go       the provider
  <name>_test.go  unit tests + providertest.Run against a fake
  README.md       scheme, ref grammar, auth, what is verified
```

The `go.mod` requires the core module and, in the monorepo, points at it with a replace directive:

```text
module github.com/xavidop/mamori/providers/<name>

go 1.26

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
```

Consumers install it on its own, so the SDK dependency is opt-in:

```bash
go get github.com/xavidop/mamori/providers/<name>
```

## Next

- [Resolve and errors](/docs/writing-a-provider/resolve/) - implement `Resolve` and map backend errors to kinds.
- [Watch and batch](/docs/writing-a-provider/capabilities/) - the optional `WatchableProvider` and `BatchProvider`.
- [Conformance](/docs/writing-a-provider/conformance/) - the required `providertest.Run` case and the acceptance checklist.
- [Providers](/docs/providers/) - the built-in provider catalog.
