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

Implement stdlib `io.Closer`, not a mamori-specific interface, when your provider holds something worth releasing: a dialed connection, a pool, a streaming client, a background refresh goroutine. mamori never calls it - see [Who closes a provider](/docs/writing-a-provider/#who-closes-a-provider) for why ownership stays with whoever constructed the provider - but the [conformance kit](/docs/writing-a-provider/conformance/) type-asserts for it and, when present, automatically checks that it is idempotent, safe with no prior `Resolve`, concurrency-safe, and terminal. It cannot generically check that `Close` leaves a caller-injected client open, since `Config` has no generic notion of "the client I handed you" - write your own unit test for that rule.

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

Two things about that snippet are worth spelling out.

**`p.closed = true` is set unconditionally, before the ownership check.** That is the part that actually matters, and it is what makes `Close` terminal on the two paths that release nothing: a provider that never built a client, and one handed an injected client it does not own. Setting the flag after an early `return nil` on either path would leave that provider quietly alive after `Close`, still resolving against a client its caller has just been told to stop using, which is a rule-four violation whether or not any test happens to catch it.

**Two prologue shapes ship in-tree, and both are correct.** Most providers do what this snippet does and rely on the nil/ownership checks for idempotency; nine (`gcp`, `gcs`, `firestore`, `mysql`, `split`, `unleash`, `growthbook`, `firebase-rc`, `configcat`) open with an explicit `if p.closed { return nil }` instead. If you read one of those and then this page, you have not found a contradiction. The snippet above is the recommended shape for a new provider, the early-return shape is fine, and the rule either must satisfy is the one above: `p.closed = true` set unconditionally before any ownership check.

And note what `Close` does **not** do: it does not stop a `Watch` that is already running. Only that watch's own context does. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for what each provider's already-running watch actually does once its provider closes, which is not one behavior.

Next: prove it all with the [Conformance](/docs/writing-a-provider/conformance/) kit.
