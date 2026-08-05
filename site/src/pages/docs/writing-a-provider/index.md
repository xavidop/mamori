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

`Close` and a running `Watch` are two independent shutdown paths, on purpose. Cancelling a watch's `context.Context` is the only thing meant to stop it; giving `Close` a second, competing way to end a watch would create an ordering question with no good answer. So `Close` never deliberately reaches into an in-flight `Watch` to end it. What actually happens to that watch once the provider closes, though, is not one behavior - it is worth knowing which of these your provider does rather than assuming the comfortable case:

- **Polling watches degrade to an error stream.** `pollWatch`'s loop resolves on every tick through the same closed-gated accessor `Resolve` uses, so a post-`Close` tick reports `mamori.ErrUnavailable`, emits it as `Update{Err: err}`, and the loop keeps polling and keeps erroring until its own context is cancelled.
- **postgres depends on which pool it is watching through.** Its `Watch` captures its backend once via `backendFor`, before the loop starts, and every cycle after that calls the backend directly - it never re-enters `backendFor` and so never passes the closed gate again. With a self-opened pool, `Close` closes that pool, but only its idle connections; the `LISTEN` connection the watch already holds survives and keeps receiving `NOTIFY`s, so each cycle's follow-up `SELECT` fails to acquire a connection instead, with an error that `classifyPostgres` passes through unclassified rather than mapping to `mamori.ErrUnavailable` - a real error stream, just not the sentinel a reader would reasonably check for. With a pool injected through `WithPool`, `Close` never touches it at all, so the watch keeps serving live values, the same shape as k8s below.
- **etcd and redis are worse than an error stream, when the client is self-dialed.** `Close` only tears down a client this provider dialed itself (`WithClient` leaves an injected client untouched, and that watch keeps delivering live events same as postgres's injected-pool case). On a self-dialed client, though, closing it closes every open watch channel (etcd) or ends the subscription's channel (redis). Both providers' watch loops treat a closed channel as a plain, silent return, with no error emitted - and mamori's own reconciler forwarder does the same with the resulting closed `Update` channel: it returns without ever calling `OnError`. So a watch on a self-dialed etcd or redis client that is already running when `Close` runs usually goes silently dead: `Get()` keeps serving the stale value, with nothing to distinguish "closed out from under you" from "quietly healthy" until you separately cancel that watch's own context. (etcd can, less commonly, deliver one final `Update{Err: ...}` first if its internal watch loop happens to exit through its error path rather than its context-done path - the silent case dominates but is not a guarantee.)
- **k8s goes the other way: it keeps serving live values.** Its `Watch` captures its own clientset reference once, before the loop starts, and delivers `Added`/`Modified` events straight from the watch stream without ever revisiting the closed-gated accessor. `Close` on this provider also never invalidates that clientset - it only evicts idle HTTP connections, which redial on demand. So a k8s watch that was already running keeps reporting real cluster changes after `Close`, indefinitely (only the snapshot re-emitted each time the watch reconnects goes through `Resolve`, and will report `mamori.ErrUnavailable` there without ending the watch).

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
