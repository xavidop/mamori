# Finding: `mamori.Update` carries no read time

**Date:** 2026-07-29
**Status:** finding, not a design. No change proposed.
**Scope:** `provider.go` (`Update`), and anything that ever adds a second writer to a value

---

## The property

`mamori.Update` (`provider.go`) is what a `WatchableProvider` delivers:

```go
type Update struct {
	Value Value
	Err   error
}
```

It records **what** the provider observed. It does not record **when the
observation happened**. `Value.Version` is the backend's revision identifier,
not a read timestamp, and providers are explicitly permitted to leave it empty
(`value.go`, `Version`'s doc comment).

So given two `Update`s, or an `Update` and a value obtained some other way,
nothing in the type can tell you which reflects the **more recent read** of the
backend.

## Why it does not matter today

Every value mamori holds has exactly one writer.

In core, `engine.observed` is written only by the reconciler goroutine. In the
config server, `resolve.go`'s doc comment states it directly: *"a single
`mamori.WatchRef` goroutine owns writing it (via `resolverState.apply`)."*

One writer means one sequence of reads, delivered in the order they were made.
Ordering is total and free, and the missing timestamp costs nothing.

## When it would matter

The moment anything else can publish a value for the same ref.

Concretely, that is any feature where a second source of truth resolves the same
ref and applies the result: an operator-triggered refresh, a cache warm, a
manual override, a second watch. Then two reads race, and **serialising the
writes does not order the reads they carry**.

The failure mode is specific and worth stating exactly, because it is not the
one people expect:

- Source A reads the backend at T1 and gets `v1`. Delivery is queued.
- Source B reads the backend at T2 > T1 and gets `v2`. It publishes `v2`.
- A's queued `v1` is then applied, **republishing the older read over the newer
  one**.

A caller told "it is now `v2`" observes `v1` afterwards. Nothing logs it. The
race detector is clean, because there is no data race: the publish is a single
atomic store, and the writes are correctly serialised. The bug is entirely in
the *reads*, which nothing orders.

This was demonstrated deterministically, 30 runs out of 30, during work on a
config-server refresh that has since been dropped. It is recorded here because
the property outlives that feature.

## What partially mitigates it

A second writer can drain and apply everything already delivered before it
performs its own read. That buys one bounded guarantee:

> Everything the watch has **delivered** before the second read begins is
> applied before it.

What it cannot cover is an update **read before but delivered after** that
instant. For a synchronous sender like the polling adapter (`poll.go`) that
window is nil. For a native `WatchableProvider` pushing asynchronously (Vault
leases, Kubernetes informers, Consul blocking queries), it is genuinely open and
as wide as the delivery lag.

Discarding updates that arrive during a read is **not** a fix. It trades a
possible revert for a certain dropped update, and for a change-driven watch a
dropped update means stale indefinitely.

## What would close it

A read timestamp on `Update`, so that a publish can refuse to overwrite a newer
read with an older one:

```go
type Update struct {
	Value Value
	Err   error
	// ReadAt is when the provider observed this value.
	ReadAt time.Time
}
```

That is a change to a public interface **every provider implements**, and it
raises questions this note deliberately does not answer: whether the field is
optional (and what a zero value means for ordering), whether the conformance kit
should require it, and whether providers can even report it honestly for a
backend that batches or caches internally.

## The point of writing this down

Not to argue for the change. Today mamori does not need it, and adding a field to
a 32-provider interface for a hazard nothing currently reaches would be
premature.

The point is that **the single-writer invariant is load-bearing in a way its
current doc comments do not say.** They explain that one writer avoids
interleaved writes. They do not say that one writer is also the *only* thing
ordering the reads, and that the second property silently disappears the moment
a second writer is added — while the first, more obvious one appears to be
preserved.

Anyone adding that second writer should read this first.
