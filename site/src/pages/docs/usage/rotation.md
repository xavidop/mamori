---
layout: ../../../layouts/DocsLayout.astro
title: Rotation safety
---

# Rotation safety

`PreApply` proves a rotated credential actually works before it becomes the
config your application serves. Reach for it when validation can only tell you
a value is well-formed, not that it is *good*.

## Why `OnChange` is not enough

By the time `OnChange` runs, `Get()` already returns the new value:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	pool.Rotate(ev.New.DBPassword.Reveal()) // too late if this password is wrong
})
```

A password the database rejects still passes `validate:"required"`. What is
missing is a check that runs *before* the swap and can say no.

## Quick start

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
		}
	}),
)
```

Keep the `ev.Changed` guard. Without it the hook re-pings the database every
time an unrelated field changes, such as a log level.

## Proving a value assembled from several fields

`PreApply` proves whatever `ev.New` holds at the moment it runs. If a field on `ev.New` is a plain local you assemble yourself after `Get()` - a DSN built from a host, a user, and a password - it was built once, at wiring time, and `PreApply` never sees it rebuilt; see [Derived fields](/docs/usage/derived-fields/) for that failure in full.

[`WithDerive`](/docs/usage/derived-fields/) installs a hook that rebuilds such a field on every applied update, before `PreApply` runs:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithDerive(func(c *Config) error {
		c.DSN = secret.NewString(fmt.Sprintf(
			"postgres://%s:%s@%s/app", c.User, c.Pass.Reveal(), c.Host))
		return nil
	}),
	mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
		if !ev.Changed("Pass") {
			return nil
		}
		return pool.Ping(ctx, ev.New.DSN.Reveal())
	}),
)
```

`WithDerive` and `PreApply` together are what make a rotated credential both **rebuilt** and **proven**: the derive reassembles the DSN from the new password, and because it runs first, `PreApply` proves the rebuilt DSN rather than the one it replaced. Without the derive, `PreApply` would still be gated correctly, just against a DSN nobody ever rebuilt.

## What a rejection does

- `Get()` keeps returning the last valid config.
- `OnChange` does not fire.
- `OnError` receives a `*PreApplyError`.
- The next upstream **change** produces a fresh candidate and the hook runs
  again. There is no periodic retry of the same rejected value, since polling
  only emits when the value actually changes.

## The timeout is a rejection, not an acceptance

`WithPreApplyTimeout` defaults to 10 seconds and cannot be removed.

```go
mamori.WithPreApplyTimeout(30 * time.Second)
```

The hook runs on the reconciler goroutine, so an unbounded hook stuck on a
hanging backend would stall every field's updates, not just its own check.

On timeout mamori does not know whether the candidate works, so it rejects. A
hook that always times out stalls updates loudly rather than quietly serving
unverified configuration.

## Do not call back into the same Watcher

The hook runs on the reconciler goroutine, which is what lets it block the
swap. `Pin`, `PinCurrent`, `Unpin`, and `Refresh` are commands serviced by that
same goroutine, so calling one from inside the hook asks it to answer while it
is busy being your hook.

**This applies to `OnError` too, and that is the one people hit**, because
"the reload was rejected, retry it" is a natural thing to write there. mamori
refuses the call rather than hanging:

| Called from inside `PreApply` or `OnError` | Result |
| --- | --- |
| `Get()` | Works. The supported way to read from either. |
| `Pin(v)` | `ErrReentrantCall`. Nothing is pinned. |
| `PinCurrent()` | Returns `0`, which never collides with a real version. Nothing is pinned. |
| `Unpin()` | Does nothing. It has no error to return. |
| `Refresh(ctx)` | `ErrReentrantCall`. Nothing is re-resolved, whatever `ctx` you pass. |

`OnChange` is the exception and is safe: it runs on its own goroutine, so all
of these work normally from there.

```go
mamori.OnError(func(err error) {
	// Refused, not hung: this callback is running ON the goroutine that
	// would have to service the refresh.
	if err := w.Refresh(context.Background()); errors.Is(err, mamori.ErrReentrantCall) {
		// refresh from another goroutine, or let the next reconciliation carry it
	}
})
```

A `Pin` from an unrelated goroutine that merely overlaps a running hook is
unaffected: it waits and is serviced normally.

## It runs on the initial load too

`PreApply` gates `Watch`'s first resolve and every `Load`, not only later
rotations. A rejection there makes `Watch` or `Load` return the
`*PreApplyError` with no watcher started.

`ev.Fields` is populated on that first call, so `ev.Changed("DBPassword")` is
true for every field set at startup and the guard above verifies the very first
credential. `ev.Old` is the zero value of `T`, since nothing was serving yet.

A hook typed for a different config than the one passed to `Watch[T]` fails
loudly with an error wrapping `ErrInvalid`, rather than silently leaving the
gate open.

## Accepting both credentials during a rotation window

A service that *accepts* credentials rather than presenting them needs both the
old and new one to work briefly. `WithHistory(1)` covers this:

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

The cost is worth stating plainly: a retained snapshot holds a full copy of
`T`, so a rotated-out credential stays in process memory for as long as you
retain it. That is why `WithHistory` defaults to `0`, and why `1` is right
here: it is the smallest window covering the overlap.

## See also

- [Derived fields](/docs/usage/derived-fields/) - `WithDerive`, so a value assembled from several fields survives a rotation too.
- [Forcing a refresh](/docs/usage/refresh/) - `Refresh`, which `PreApply` gates too.
- [Watch for changes](/docs/usage/watching/) - `Watch`, `Get`, `OnChange`, `OnError`.
- [Snapshots and pinning](/docs/usage/snapshots/) - `WithHistory` and pinning.
