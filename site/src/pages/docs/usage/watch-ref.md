---
layout: ../../../layouts/DocsLayout.astro
title: Watching one ref
---

# Watching one ref

```go
func WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update
```

`WatchRef` watches a single ref and hands you back a raw channel of `Update`s. There is no config struct anywhere in this path: no fields, no `source:` tags, nothing that looks like `Load` or `Watch[T]`. You give it a `Provider` and a `Ref`, and you get a channel.

## When to use it

Reach for `WatchRef` when you want to watch one value, not maintain a struct's worth of them. The clearest example is [the config server](/docs/server/), which uses it internally, and that is exactly why `WatchRef` is exported: a caller gets the identical native-watch-or-poll selection that the reconciler already performs per field inside `Watch[T]`, rather than a second, independently maintained copy of the same decision. If your own code has one ref it cares about, you are in the same position the server is.

## What it does not give you

This is the section to read before reaching for `WatchRef` instead of `Watch[T]`. Compared to `Watch[T]`, you lose:

- **Validation.** There is no struct, so there are no `validate:` tags, and nothing checks the values you receive.
- **The atomic swap and last-good snapshot.** You get raw updates as they arrive, including error updates, and it is on you to decide what each one means for whatever state your code is holding. There is no `Get()` quietly serving the last good value while you work that out.
- **The `PreApply` gate.** Nothing stands between an update and your code acting on it.
- **`Change` diffing and `Changed()`.** An `Update` is just a value or an error; there is no field to diff against a previous one, and no `Changed(path)` to ask about it.
- **`Status()` and `Health()`.** There is no watcher object here, so there is nothing to report readiness or staleness for.
- **History and pinning.** There is no snapshot store, so there is nothing to pin to or roll back through.

If any of that list is what you actually need, you want `Watch[T]` instead. See [Watching](/docs/usage/watching/).

## Which options apply

`WatchRef` takes `opts ...Option`, the same `Option` type `Load` and `Watch` take, so you never need a second, narrower vocabulary of options just for a single ref. But only a handful of them actually reach `WatchRef`: `WithClock`, `WithPollInterval`, `WithJitter`, and the `WithBackoff` window. Those are the fields the polling adapter itself reads. Everything else in the `Option` surface, including `WithValidator` and `PreApply`, is accepted without complaint and simply has no effect here, since there is no validation step or gate for it to attach to. See the [Options reference](/docs/usage/options/) for the full surface and where each option does and does not apply.

## The channel contract

The returned channel is closed when `ctx` is cancelled, and that closure, not an error value, is what tells you `WatchRef` is done. A transient failure arrives as an `Update` with a non-nil `Err`; the channel stays open and further updates keep arriving after it.

```go
ch := mamori.WatchRef(ctx, provider, ref, mamori.WithPollInterval(30*time.Second))
for u := range ch {
	if u.Err != nil {
		log.Printf("watch error: %v", u.Err)
		continue
	}
	log.Printf("new value, version %s", u.Value.Version)
}
// This loop only exits once ctx is cancelled and the channel closes.
```

## Native watch versus polling

If `p` implements `WatchableProvider`, `WatchRef` watches it natively. If that native watch fails to start, `WatchRef` falls back to polling rather than failing outright. Any provider that does not implement `WatchableProvider` is polled from the start, with no attempt at a native watch.
