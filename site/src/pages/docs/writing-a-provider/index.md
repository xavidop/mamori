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

A provider has a `Close` method when it holds a releasable resource: a dialed connection, a pool, a streaming client, a background refresh goroutine. If you are writing one, that is the test to apply.

Calling `Close` is always safe where it exists, even when the provider had nothing to release. `sqlite` is the case in point: it opens and closes a connection inside every `Resolve`, so it holds nothing between calls, but it still has a `Close` so that shutting down every database provider together does what you expect.

### The contract

A provider `Close` is:

- **Idempotent.** Two calls, no error, no panic.
- **Safe with no prior use.** `New` followed by `Close`, with no `Resolve` in between, must not dial. Lazily built clients and pools make this a real path, not a hypothetical.
- **Concurrency-safe.** Callable while a `Resolve` is in flight.
- **Terminal.** After `Close`, `Resolve` returns an error satisfying `errors.Is(err, mamori.ErrUnavailable)`, locally and fast, without touching the backend.
- **Never the owner of an injected client.** A pool or client passed in through an option such as `WithPool`, `WithDB`, `WithClient` or `WithHTTPClient` belongs to the caller. Track what you built yourself and release only that.

Use stdlib `io.Closer` rather than a mamori-specific interface. The [conformance kit](/docs/writing-a-provider/conformance/) picks it up from there and checks the first four rules for you.

### Close does not stop a Watch

`Close` never ends a `Watch` that is already running. Cancel the watch's context to stop it, and do that before you close the provider:

```go
ctx, cancel := context.WithCancel(context.Background())
w, err := mamori.Watch[Config](ctx, mamori.WithProvider(p))
// ...
cancel()    // stops the watch
w.Close()   // releases what mamori created
p.Close()   // releases what you created
```

Close the provider while its watch is still running and the watch does not stop; what it does instead depends on the provider. One of the outcomes is silent, which is the reason to care:

| What you see afterwards | Which providers |
| --- | --- |
| Errors arrive at your error handler and keep arriving | Any provider mamori polls, which is most of them |
| Live values keep arriving, as if nothing had happened | k8s, and any provider whose client or pool you injected yourself with `WithClient` or `WithPool` |
| Errors arrive, but carry the backend's own error rather than `mamori.ErrUnavailable`, so a check on that sentinel misses them | postgres, on a pool it opened itself |
| Nothing arrives. Your error handler never fires and `Get()` keeps serving the last value indefinitely | etcd and redis, on a client they dialed themselves |

That is a difference between providers rather than a promise mamori makes, so check the page of any provider not named here instead of assuming. Cancelling the context is the one shutdown path that behaves the same everywhere.

This is a genuine, known inconsistency across providers, not a documented guarantee - do not assume a comfortable middle ground for a provider not named above without reading its `Watch` for yourself. The one thing true everywhere: cancelling the watch's own context is the only shutdown path you can rely on. Never reach for `Close` to stop a `Watch`.

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
