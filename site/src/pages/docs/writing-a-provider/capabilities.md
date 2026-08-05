---
layout: ../../../layouts/DocsLayout.astro
title: Watch and batch
---

# Watch and batch

Implement these optional interfaces only when the backend supports them. mamori type-asserts each at runtime and uses it automatically: a `WatchableProvider` is watched natively, one without it is wrapped in a polling adapter that calls `Resolve` on a schedule (so never fake a `Watch`); a `BatchProvider` gets `ResolveBatch`, otherwise mamori falls back to individual `Resolve` calls. Adding a capability later never requires a consumer change.

## Watch for native changes

Implement `WatchableProvider` **only** for backends with native change notification (Vault leases, Kubernetes informers, Consul blocking queries, `fsnotify`). Providers without native watch are wrapped in the polling adapter, so never fake a `Watch` with an internal ticker.

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

Deliver a new value as `mamori.Update{Value: v}` and a transient error as `mamori.Update{Err: err}` (the channel stays open; mamori keeps the last-good value). Closing the channel signals termination, which must happen on `ctx` cancellation.

## Resolve batches in one call

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

## Release a held resource

Implement stdlib `io.Closer`, not a mamori-specific interface, when your provider holds something worth releasing: a dialed connection, a pool, a streaming client, a background refresh goroutine. The caller who constructed the provider calls it; mamori never does ([why](/docs/writing-a-provider/#who-closes-a-provider)).

Adding the method is all you need to do to get it tested. The [conformance kit](/docs/writing-a-provider/conformance/) finds it and checks that it is idempotent, safe with no prior `Resolve`, concurrency-safe, and terminal. The one rule it cannot check for you is that `Close` leaves a caller-injected client open, so write a unit test for that yourself.

```go
// Optional. Only implement this if there is something to release.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Every later Resolve, and any Watch started after Close, now reports
	// mamori.ErrUnavailable. A Watch already running is NOT covered by that:
	// see "Close does not stop a Watch".
	p.closed = true
	if !p.ownClient || p.client == nil {
		return nil // never built, or injected by the caller: nothing to release
	}
	err := p.client.Close()
	p.client = nil
	return err
}
```

**Set `p.closed = true` before the ownership check, not after.** It is the easiest rule to get wrong, because the two paths that release nothing are the two that skip it: a provider that never built a client, and one holding a client the caller injected. Put the flag after an early `return nil` on either path and that provider stays alive after `Close`, still resolving against a client its caller has been told to stop using.

An `if p.closed { return nil }` at the top of the method works equally well, as long as the flag is still set on every path that reaches the end.

`Close` does not stop a `Watch` that is already running; only that watch's own context does, and what the watch does in the meantime varies by provider. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch).

Next: prove it all with the [Conformance](/docs/writing-a-provider/conformance/) kit.
