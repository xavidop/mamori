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
- The rejected value is **not** withdrawn from the engine's observed state, but the per-field *applied* versions are rolled back, so the same value is diffed and re-gated rather than looking already applied. The next upstream **change** produces a fresh candidate and the hook runs again. There is no periodic retry of the rejected value on its own: polling only emits when the value actually changes, so a poll that re-reads the same rejected value produces nothing and the hook does not run again.

## The timeout is mandatory, and exceeding it is a rejection

`WithPreApplyTimeout` bounds how long a hook may run, defaulting to 10 seconds:

```go
mamori.WithPreApplyTimeout(30 * time.Second)
```

The bound cannot be removed. The hook runs on the reconciler goroutine, the same goroutine that services every other field's updates, publishes `Status`, and handles pin/unpin commands. An unbounded hook (one stuck on a hanging backend) would wedge all of that, not just its own check.

Exceeding the budget is a **rejection, not an acceptance**. On timeout, mamori does not know whether the candidate actually works, and applying it anyway would defeat the point of having a gate at all. A hook that always times out therefore stalls updates loudly, once per attempt, rather than quietly serving unverified configuration.

## Do not call back into the same Watcher

The hook runs on the reconciler goroutine, which is what lets it block the swap in the first place. That has one consequence you have to design around: `Get()` is a lock-free atomic load, so it is safe to call from inside the hook, but `Pin`, `PinCurrent`, `Unpin`, and `Refresh` are commands sent to, and serviced by, that very same goroutine. Calling one of them from inside a `PreApply` hook asks for a reply that only the goroutine it is currently occupying could ever send.

**This applies to `OnError` too, and that is the one people hit.** `OnError` is not delivered through the queue `OnChange` uses; it runs *inline* on the reconciler goroutine, so errors are never dropped. That puts an `OnError` callback in exactly the same position as a hook - and "the reload was rejected, retry it" is a natural thing to write there, with `w.Refresh` as the obvious way to write it.

`OnChange` is the exception, and it is safe: it is delivered from the dispatch queue on its own goroutine, so `Pin`, `Unpin`, and `Refresh` from inside an `OnChange` handler are ordinary calls, serviced in the ordinary way.

`WithPreApplyTimeout` does not rescue any of this. The timeout only cancels the `ctx` handed to the hook; `Pin`, `PinCurrent`, and `Unpin` take no `ctx` at all, and `Refresh` takes the *caller's* `ctx`, not the hook's, so there is nothing on any of their sides for the hook's timeout to cancel. An `OnError` callback has no timeout of its own at all.

**mamori detects this and fails the call instead.** Until it did, a callback stuck this way wedged the whole watcher permanently, not just its own check: no reconciliation, no `OnChange`, no further `OnError`, nothing until something outside cancelled the watcher (`w.Close()`, or the parent context passed to `Watch`). Now the call returns immediately, changes no pin state, and the callback carries on:

| Called from inside a `PreApply` hook or an `OnError` callback | Result |
| --- | --- |
| `Get()` | Works, and is the supported way to read from either. It returns whatever `Get()` returns anywhere else at that instant: normally the snapshot the candidate would supersede, since the swap has not happened yet - but while the watcher is *pinned*, the pinned snapshot, which is not the one the candidate supersedes. |
| `Pin(v)` | Returns `ErrReentrantCall`. Nothing is pinned. |
| `PinCurrent()` | Returns `0`. Versions start at 1, so `0` never collides with a real one. Nothing is pinned. |
| `Unpin()` | Does nothing. Its signature has no error to return, so it leaves the watcher pinned and `Pinned()` still reports so. |
| `Refresh(ctx)` | Returns `ErrReentrantCall`. Nothing is re-resolved, no matter what `ctx` is - see [Forcing an immediate refresh](#forcing-an-immediate-refresh) below. |

```go
mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
	_ = w.Get() // fine: a lock-free atomic load, safe from inside the hook

	// Refused, not hung: returns ErrReentrantCall at once and pins nothing.
	if err := w.Pin(1); errors.Is(err, mamori.ErrReentrantCall) {
		// call Pin from another goroutine instead
	}
	return nil
})

mamori.OnError(func(err error) {
	// Same refusal, same reason: this callback is running ON the goroutine
	// that would have to service the refresh.
	if err := w.Refresh(context.Background()); errors.Is(err, mamori.ErrReentrantCall) {
		// let the next reconciliation retry, or refresh from another goroutine
	}
})
```

The detection keys on *which* goroutine is inside the callback, not merely that one is. A `Pin` issued from an unrelated goroutine that happens to overlap a running hook or callback is not affected: it waits for the reconciler to come free and is then serviced normally, exactly as before.

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

```go
func (w *Watcher[T]) Refresh(ctx context.Context) error
```

`w.Refresh(ctx)` re-resolves every field right now, bypassing poll intervals, and **blocks until the resulting snapshot has been applied or rejected**. That block is deliberate, not something to route around: a SIGHUP handler wants to know whether the reload it just triggered actually worked, not merely that a request for one was queued.

```go
sighup := make(chan os.Signal, 1)
signal.Notify(sighup, syscall.SIGHUP)

for range sighup {
	switch err := w.Refresh(ctx); {
	case err == nil:
		log.Println("reload applied")
	case ctx.Err() != nil:
		// The wait was abandoned, not the reload. Whether it applied is
		// unknown from here; w.Status() reports what actually happened.
		log.Printf("stopped waiting for the reload: %v", err)
	default:
		log.Printf("reload rejected, still serving the previous config: %v", err)
	}
}
```

Splitting those last two cases is not pedantry. A cancelled `ctx` returns `ctx.Err()` **while the reconciler goes on to apply the reload anyway** (see below); treating every non-nil error as a rejection would log "still serving the previous config" for a reload that in fact landed.

`Refresh` returns `nil` in the two cases that both mean "`Get()` is current": a snapshot was applied, or nothing had actually changed. It returns the *rejection reason* when the candidate was refused:

- a field failed validation,
- `PreApply` rejected the candidate - a `*PreApplyError`, exactly as in [What a rejection does](#what-a-rejection-does) above,
- or a field is blocked by `onfail:"fail"` and stayed blocked.

The remaining non-nil returns are not rejections at all, and should not be reported as one: `ctx.Err()` means *you* stopped waiting (the reload proceeds regardless), and a closed-watcher error means there was no reconciler left to ask.

**`Refresh` does not bypass `PreApply`.** A forced refresh is gated exactly like any other reconciliation. Skipping the gate on the one call an operator reaches for right after a rotation - the moment a gate matters most - would defeat the point of having it.

`ctx` bounds the wait, not the work. Cancelling it makes `Refresh` return `ctx.Err()` and stop waiting, but the command already handed to the reconciler still re-resolves and applies (or is rejected) as usual - there is no way to recall it, and no half-applied snapshot either way. Called after `Close`, `Refresh` returns the same closed-watcher error `Pin` does.

While the watcher is pinned, a refresh still re-resolves, still runs the `PreApply` gate, and still advances `Live` and history - it just does not move `Get()`, which is what the pin is for. It returns `nil` in that case too: the candidate was applied as far as the pin allows, and `Unpin` will publish it. `Refresh` never silently unpins for you.

### The reentrancy hazard applies to `Refresh` too

`Refresh` is serviced by the reconciler goroutine - the same one a `PreApply` hook occupies for the duration of its call, and the same one an `OnError` callback occupies for the duration of *its* call. Calling `w.Refresh` from inside either would ask that goroutine to service a command while it is busy being the callback, so it is refused rather than left to hang, exactly like `Pin`, `PinCurrent`, and `Unpin`: it returns `ErrReentrantCall` immediately, having re-resolved nothing. Giving it its own `ctx` does not rescue it - `Refresh(context.Background())`, the obvious thing to reach for, would simply block until `Close`, since nothing else would ever free the goroutine it is waiting on.

`OnError` is the one to watch for here. Retrying a rejected reload from the callback that told you it was rejected is the natural shape to reach for, and it is the shape that wedges: issue the `Refresh` from another goroutine, or simply let the next reconciliation carry it. See [Do not call back into the same Watcher](#do-not-call-back-into-the-same-watcher) above for the full rule and what each command returns.

### What running it actually costs

`Refresh` runs *on* the reconciler goroutine, not merely through it. For however long the walk over every field takes, `Pin`/`Unpin` go unserviced, watch updates back up on their channel, the debounce timer cannot be observed firing, and no new `Report` is published. That is the honest cost of a synchronous, whole-config re-resolve, which is why `Refresh` is for an operator-triggered reload - a SIGHUP, an admin action - not something to call in a hot path or a loop.

Its round trips also bypass the same resolve path a watch-delivered update goes through, so they are invisible to a configured [`WithTracer`](/docs/opentelemetry/) span and [`WithMeter`](/docs/opentelemetry/) resolve metric, and they skip `BatchProvider` grouping - a scheme that normally batches several fields into one round trip pays one round trip per field here instead. That cost is inherited from the same per-chain seeding walk `Watch` already runs once at startup; `Refresh` simply runs it for every field, on demand, which is the whole point of a forced refresh.

For a field resolved through a `mamori://` ref, `Refresh` re-reads the [config server's](/docs/server/) current value - it does not reach past the server to force its upstream to re-resolve. See [Config server](/docs/server/#refresh-re-reads-the-server-not-the-upstream) for why that boundary exists and what is planned for it. If you want an HTTP-triggered refresh instead of a SIGHUP, there is also no route for one on the [admin endpoint](/docs/observability/admin/#no-post-refresh) - a different, simpler reason: that surface is deliberately read-only. Mount your own handler there instead.

## See also

- [Watch for changes](/docs/usage/watching/) for `Watch`, `Get`, `OnChange`, and `OnError`.
- [Snapshots and pinning](/docs/usage/snapshots/) for `WithHistory`, `History`, and pinning.
- [Loading and watching](/docs/usage/) for the option walkthrough.
- [Admin endpoint](/docs/observability/admin/) for why there is no `POST /refresh` there, and how to mount your own authorized one.
- [Config server](/docs/server/) for what `Refresh` means, and does not mean, for a `mamori://`-resolved field.
