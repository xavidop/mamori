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

## Who closes a provider

mamori never closes a provider. `Watcher.Close()` releases only what mamori itself created: the watch goroutines, the callback queue, and the admin server. A provider instance belongs to whoever constructed it.

That rule exists because `Register` stores a provider in a process-global map, normally from a package `init`, and every `Watcher` in the process shares that one instance. A `Watcher.Close()` that released it would hand the next `Watch` call a dead client.

So a provider that holds a connection is yours to close:

```go
p := postgres.New(postgres.WithDSN(dsn))
defer p.Close()      // you own p

w, err := mamori.Watch[Config](ctx, mamori.WithProvider(p))
defer w.Close()      // closes only what mamori created
```

Providers that hold no releasable handle (`env:`, `file://`, `aws-sm://`, and others) have no `Close` at all, so there is nothing to forget.

### Which providers get a Close

A provider earns a `Close` method when it holds a releasable resource: a dialed connection, a pool, a streaming client, a background refresh goroutine. `providers/sqlite` is the one deliberate exception. It opens and closes a fresh `*sql.DB` inside every `Resolve`, so it holds nothing between calls, yet it still ships a terminal-only `Close`: it sits beside `providers/postgres` and `providers/mysql`, and a caller sweeping `Close` across the database providers at shutdown would be surprised to find sqlite alone still serving. So the rule is: a provider gets `Close` when it holds a releasable resource, or when its siblings do.

### The contract

A provider `Close` is:

- **Idempotent.** Two calls, no error, no panic.
- **Safe with no prior use.** `New` followed by `Close`, with no `Resolve` in between, must not dial. Lazily built clients and pools make this a real path, not a hypothetical.
- **Concurrency-safe.** Callable while a `Resolve` is in flight.
- **Terminal.** After `Close`, `Resolve` returns an error satisfying `errors.Is(err, mamori.ErrUnavailable)`, locally and fast, without touching the backend.
- **Never the owner of an injected client.** A pool or client passed in through an option such as `WithPool`, `WithDB`, `WithClient` or `WithHTTPClient` belongs to the caller. Track what you built yourself and release only that.

`io.Closer` is how the [conformance kit](/docs/writing-a-provider/conformance/) discovers that a provider holds a releasable resource; that type assertion is `providertest`'s job, not core's. mamori itself never asserts a `Provider` for `io.Closer` and never calls `Close` - only the caller who constructed the provider does.

### Close does not stop a Watch

`Close` and a running `Watch` are two independent shutdown paths, on purpose. Cancelling a watch's `context.Context` is the only thing that ever stops it; giving `Close` a second, competing way to end a watch would create an ordering question with no good answer. So `Close` does not reach into an in-flight `Watch` and end it. Instead, once a provider is closed, the accessor its `Resolve` (and a native `Watch`'s reconnect path) goes through reports `mamori.ErrUnavailable` locally. A polling watch keeps polling and keeps emitting that error on every tick; a native watch that needs to reconnect hits the same closed provider and reports the same error. Either way the watch degrades to an error stream rather than stopping outright, and it only ends when its own context is cancelled.

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
- [HTTP core](/docs/writing-a-provider/httpcore/) - if your backend is a REST API, do not hand-roll the HTTP: `providers/httpcore` does request building, auth, classification, conditional GET, and body hygiene.
- [Conformance](/docs/writing-a-provider/conformance/) - the required `providertest.Run` case and the acceptance checklist.
- [Providers](/docs/providers/) - the built-in provider catalog.

If your provider resolves secrets, tell the analyzer about its scheme so a plain `string` holding one still gets flagged: [`mamori vet --secret-schemes=<yours>`](/docs/cli/vet/).
