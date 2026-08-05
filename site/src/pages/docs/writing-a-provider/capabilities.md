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

Implement stdlib `io.Closer`, not a mamori-specific interface, when your provider holds something worth releasing: a dialed connection, a pool, a streaming client, a background refresh goroutine. mamori never calls it - see [Who closes a provider](/docs/writing-a-provider/#who-closes-a-provider) for why ownership stays with whoever constructed the provider - but the [conformance kit](/docs/writing-a-provider/conformance/) type-asserts for it and, when present, exercises the full `Close` contract automatically.

```go
// Optional. Only implement this if there is something to release.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true // makes every later Resolve/Watch report mamori.ErrUnavailable
	if !p.ownClient || p.client == nil {
		return nil // never built, or injected by the caller: nothing to release
	}
	err := p.client.Close()
	p.client = nil
	return err
}
```

Next: prove it all with the [Conformance](/docs/writing-a-provider/conformance/) kit.
