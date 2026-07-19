---
layout: ../../layouts/DocsLayout.astro
title: Middleware
---

# Middleware

Because every provider is a `Provider`, decorators from `github.com/xavidop/mamori/middleware` nest freely. They instrument the resolve path; native watch is delegated to the wrapped provider.

## Using middleware

```go
import "github.com/xavidop/mamori/middleware"

mamori.WithProvider(
	middleware.Cache(5*time.Minute,          // memoize resolves for a TTL
		middleware.Audit(logger,             // log every access, never the payload
			middleware.RateLimit(10,         // <= 10 resolves/second
				middleware.Failover(         // primary, then replicas
					primarySM, replicaSM,
				),
			),
		),
	),
)
```

| Middleware | What it does |
| --- | --- |
| `Cache(ttl, inner)` | Memoize successful resolves for `ttl`. Errors and not-found are not cached. |
| `Audit(logger, inner)` | Log scheme, ref, latency, and outcome. Never the value. |
| `Failover(primary, replicas...)` | Try primary, then each replica on a transport error. Not-found is authoritative. |
| `RateLimit(rps, inner)` | Space resolves to at most `rps` per second. |
| `Prefix(prefix, inner)` | Rewrite each ref's path under a namespace (multi-tenant). |

`Cache` and `RateLimit` accept a `WithClock` option for deterministic tests.

## Writing middleware

Middleware is just a `Provider` that wraps another `Provider`:

```go
func Trace(inner mamori.Provider) mamori.Provider {
	return &tracer{inner: inner}
}

type tracer struct{ inner mamori.Provider }

func (t *tracer) Scheme() string { return t.inner.Scheme() }

func (t *tracer) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	start := time.Now()
	v, err := t.inner.Resolve(ctx, ref)
	log.Printf("resolve %s took %s (err=%v)", ref.Raw, time.Since(start), err)
	return v, err
}

// Optionally forward Watch so a watchable inner stays watchable:
func (t *tracer) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	if wp, ok := t.inner.(mamori.WatchableProvider); ok {
		return wp.Watch(ctx, ref)
	}
	return nil, errors.New("inner is not watchable") // mamori falls back to polling
}
```

The shipped middleware uses a shared wrapper that preserves `WatchableProvider` and `BatchProvider` when the inner supports them, so decoration never silently drops a capability.
