---
layout: ../../../layouts/DocsLayout.astro
title: Rotation safety
---

# Rotation safety

`PreApply` proves a rotated value actually works before it becomes the config your application serves, instead of finding out in the request path. Reach for it whenever struct validation cannot tell you whether a credential is *good*, only whether it is *well-formed*.

## The problem: OnChange is a notification, not a gate

By the time `OnChange` runs, `Get()` already returns the new value:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.OnChange(func(ev mamori.Change[Config]) {
		if ev.Changed("DBPassword") {
			pool.Rotate(ev.New.DBPassword.Reveal()) // too late if this password is wrong
		}
	}),
)
```

If the rotated `DBPassword` does not actually open a connection, every concurrent `Get()` caller is already serving it. Validation cannot catch this: a syntactically fine password that the database rejects passes `validate:"required"` without complaint. What's missing is a check that runs *before* the swap and can say no.

## Quick start

`PreApply` runs after struct validation and before the atomic swap. Returning an error rejects the candidate:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
		if !ev.Changed("DBPassword") {
			return nil
		}
		return pool.Ping(ctx, ev.New.DBPassword.Reveal())
	}),
	mamori.OnError(func(err error) {
		var perr *mamori.PreApplyError
		if errors.As(err, &perr) {
			metrics.Inc("config_rejected")
			return
		}
		metrics.Inc("config_error")
	}),
)
if err != nil {
	log.Fatal(err)
}
defer w.Close()
```

The `ev.Changed("DBPassword")` guard matters: without it, the hook re-pings the database on every unrelated field change (a log level, a worker count) instead of only when the credential it cares about actually rotated.

## What a rejection does

Returning a non-nil error from the hook rejects the candidate outright:

- `Get()` keeps returning the last valid config; the rejected candidate is discarded.
- `OnChange` does not fire.
- `OnError` receives a `*PreApplyError` wrapping the hook's own error (or `context.DeadlineExceeded` on timeout, below).
- The engine's per-ref state is not rewound. The next upstream change or poll produces a fresh candidate and the hook runs again. There is no periodic retry of the rejected value on its own; it is re-gated only on the next change.

## The timeout is mandatory, and exceeding it is a rejection

`WithPreApplyTimeout` bounds how long a hook may run, defaulting to 10 seconds:

```go
mamori.WithPreApplyTimeout(30 * time.Second)
```

The bound cannot be removed. The hook runs on the reconciler goroutine, the same goroutine that services every other field's updates, publishes `Status`, and handles pin/unpin commands. An unbounded hook (one stuck on a hanging backend) would wedge all of that, not just its own check.

Exceeding the budget is a **rejection, not an acceptance**. On timeout, mamori does not know whether the candidate actually works, and applying it anyway would defeat the point of having a gate at all. A hook that always times out therefore stalls updates loudly, once per attempt, rather than quietly serving unverified configuration.

## Do not call back into the same Watcher

The hook runs on the reconciler goroutine, which is what lets it block the swap in the first place. That has one consequence you have to design around: `Get()` is a lock-free atomic load, so it is safe to call from inside the hook, but `Pin`, `PinCurrent`, and `Unpin` are commands sent to, and serviced by, that very same goroutine. Calling one of them from inside a `PreApply` hook asks for a reply that only the goroutine it is currently occupying could ever send.

`WithPreApplyTimeout` does not rescue this. The timeout only cancels the `ctx` handed to the hook; `Pin`, `PinCurrent`, and `Unpin` take no `ctx` at all, so there is nothing for the timeout to cancel on their side.

**mamori detects this and fails the call instead.** Until it did, a hook stuck this way wedged the whole watcher permanently, not just its own check: no reconciliation, no `OnChange`, no `OnError`, nothing until something outside the hook cancelled the watcher (`w.Close()`, or the parent context passed to `Watch`). Now the call returns immediately, changes no pin state, and the hook carries on:

| Called from inside a hook | Result |
| --- | --- |
| `Get()` | Works, and is the supported way to read from a hook. Returns the snapshot this candidate would supersede, since the swap has not happened yet. |
| `Pin(v)` | Returns `ErrReentrantCall`. Nothing is pinned. |
| `PinCurrent()` | Returns `0`. Versions start at 1, so `0` never collides with a real one. Nothing is pinned. |
| `Unpin()` | Does nothing. Its signature has no error to return, so it leaves the watcher pinned and `Pinned()` still reports so. |

```go
mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
	_ = w.Get() // fine: a lock-free atomic load, safe from inside the hook

	// Refused, not hung: returns ErrReentrantCall at once and pins nothing.
	if err := w.Pin(1); errors.Is(err, mamori.ErrReentrantCall) {
		// call Pin from another goroutine instead
	}
	return nil
})
```

The detection keys on *which* goroutine is inside the hook, not merely that one is. A `Pin` issued from an unrelated goroutine that happens to overlap a running hook is not affected: it waits for the reconciler to come free and is then serviced normally, exactly as before.

If a hook needs to compare against something other than `ev.Old` or `ev.New`, keep a reference outside the `Watcher` rather than asking the watcher for it mid-hook. Detection turns a hang into a diagnosable error; it does not make the call work.

## Runs on the initial load too

`PreApply` gates `Watch`'s initial resolve and every call to `Load`, not only later rotations. A rejection on that first load makes `Watch` and `Load` return the `*PreApplyError` with no watcher started, exactly as a validation failure would.

`ev.Fields` is populated on this first call too: it lists every field that resolved to a value, so `ev.Changed("DBPassword")` is `true` for each field set at startup. That is what makes the guard pattern above verify the very first configured credential, not just later rotations. `ev.Old` is the zero value of `T` on this call, since nothing was serving before it.

A hook typed for a different config than the one passed to `Watch[T]` or `Load[T]` is a caller bug, and it fails loudly: `Watch` and `Load` return an error wrapping `ErrInvalid` rather than silently running with the gate open.

## The credential-overlap pattern

A service that *accepts* incoming credentials, rather than presenting them, has a narrower problem: during a rotation window, both the old and new credential are momentarily valid, and a caller using either one should be accepted. `WithHistory(1)` plus `w.History()` already covers this; no dual-credential machinery is needed.

```go
w, _ := mamori.Watch[Config](ctx, mamori.WithHistory(1))

func accept(presented string) bool {
	for _, s := range w.History() { // current, then previous
		if subtle.ConstantTimeCompare(
			[]byte(presented), []byte(s.Config.APIKey.Reveal())) == 1 {
			return true
		}
	}
	return false
}
```

State the cost as plainly as the benefit: a retained snapshot holds a full copy of `T`, including whatever secret material it carried, for as long as it stays in the history window. Enabling history keeps a rotated-out credential reachable in process memory for exactly as long as you retain it, which is why `WithHistory` defaults to `0`. `WithHistory(1)` is the right setting for this pattern specifically because it is the smallest window that covers the overlap; a larger `n` widens the overlap and the exposure together. See [Snapshots and pinning](/docs/usage/snapshots/#retaining-snapshots-with-withhistory) for `WithHistory` itself.

## Forcing an immediate refresh

A way to force mamori to re-resolve right now, ahead of the next poll, is not available yet in this release.

## See also

- [Watch for changes](/docs/usage/watching/) for `Watch`, `Get`, `OnChange`, and `OnError`.
- [Snapshots and pinning](/docs/usage/snapshots/) for `WithHistory`, `History`, and pinning.
- [Loading and watching](/docs/usage/) for the option walkthrough.
