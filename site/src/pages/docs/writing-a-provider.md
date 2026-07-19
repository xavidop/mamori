---
layout: ../../layouts/DocsLayout.astro
title: Write a provider
---

# Write a provider

A provider is a small, self-contained module. The full contract lives in [`docs/PROVIDER_SPI.md`](https://github.com/xavidop/mamori/blob/main/docs/PROVIDER_SPI.md); here are the essentials.

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
	})
}
```

Inject a client interface so the kit (and your unit tests) run against an in-memory fake, with live-backend tests behind a `//go:build integration` tag. A provider that passes the kit earns a badge in the registry.
