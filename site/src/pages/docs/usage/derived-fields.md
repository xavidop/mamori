---
layout: ../../../layouts/DocsLayout.astro
title: Derived fields
---

# Derived fields

A DSN assembled from a host, a user, and a password in application code, after `Get()`, is a plain local built **once**, at wiring time:

```go
cfg := w.Get()
dsn := fmt.Sprintf("postgres://%s:%s@%s/app", cfg.User, cfg.Pass.Reveal(), cfg.Host)
pool := connect(dsn)
```

Three weeks later the password rotates. mamori does its job perfectly: `w.Get().Pass` returns the new password the instant it lands. The `dsn` variable above does not, because nothing ever asked it to. It was computed once, and mamori has no way to know that value depended on `Pass` at all. The pool keeps using a credential that is about to be revoked, and `w.Status()` reports every field perfectly healthy, because mamori's reconciliation reached every field it knows about and stopped exactly where you assembled the DSN yourself.

Building the DSN with `fmt.Sprintf` also breaks silently the moment a rotated password contains an `@` or a `/`: neither is escaped, so the resulting DSN parses into the wrong host or the wrong path.

## The fix

`WithDerive` moves that assembly inside mamori, so it happens again on every applied update instead of once, and declares which field it writes so mamori can track it like any other:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithDerive(func(c *Config) error {
		c.DSN = secret.NewString((&url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(c.User, c.Pass.Reveal()),
			Host:   c.Host,
			Path:   "/app",
		}).String())
		return nil
	}, "DSN"),
)
```

`WithDerive` installs a hook that runs on every `Load` and every reconciled update, after fields are decoded and before validation, so `DSN` above is rebuilt from whatever `User`, `Pass`, and `Host` currently hold, not from what they held when the process started.

Build the DSN with `net/url`, as above, not `fmt.Sprintf`: `net/url` escapes a password containing `@` or `/` correctly, so a rotated password containing either still produces a valid DSN instead of a silently broken one.

## Declare what it writes

The second argument to `WithDerive`, `"DSN"` above, is the dotted field path the hook writes (the same shape a `source` tag's field path uses). Declaring it is what makes `DSN` show up in `ev.Changed("DSN")` and in `Status().Fields`: mamori cannot inspect an opaque Go function to see what it assigns, so the hook states its own output. `ev.Changed("DSN")` works exactly like it does for any other field, but the `Status().Fields` entry does not: it carries only a `Path` (and `Derived: true`), never a `Ref`, `Scheme`, or `Version`, since a derived field has none of those to report - see [Observability](/docs/observability/#report-and-fieldstatus) for the full shape.

`WithDerive` can also be called with no path at all, `mamori.WithDerive(fn)`, and the hook still registers and runs; it simply writes a field nobody can see change. Declaring the path is the ordinary way to use this option, not an advanced one, because there is little reason to build a field mamori can never tell you rebuilt.

## React to it

With the write declared, `DSN` is a normal trigger in `OnChange`:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("DSN") {
		pool.Rotate(ev.New.DSN.Reveal())
	}
})
```

`ev.Changed("DSN")` is true whenever the rebuilt value differs from the one it replaced, whether that is because `Pass` rotated, `Host` moved, or any other input the hook reads changed. There is no need to separately guard on `ev.Changed("Pass")`: the derived field's own change is the signal.

## Secret hygiene

The assembled DSN embeds the password, so the target field should be a `secret.String` (or `secret.Bytes`), not a plain `string`. That is what keeps the rebuilt value redacted in `fmt`, JSON, and `slog`, the same as any other secret field. `Status()` never carries a derived field's value at all, so this matters for your own logging and error messages, not for what mamori reports.

## A derive error rejects the whole update

Returning an error from a derive rejects the whole candidate configuration, exactly as a validation failure does: `Get()` keeps returning the last valid config, and the error reaches `OnError` as a `*DeriveError`.

```go
mamori.OnError(func(err error) {
	var de *mamori.DeriveError
	if errors.As(err, &de) {
		metrics.Inc("config_derive_error")
	}
})
```

Rejecting rather than continuing is deliberate: a configuration whose derived fields did not build is not one anyone should serve, and half-applying it would produce a snapshot where some fields reflect a rotated credential and a value derived from them still reflects the old one.

## Multiple hooks run in registration order

`WithDerive` can be given more than once. Each call appends a hook rather than replacing the last one, and they run in the order you registered them, each keeping its own declared writes:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithDerive(func(c *Config) error {
		c.FullName = c.First + " " + c.Last
		return nil
	}, "FullName"),
	mamori.WithDerive(func(c *Config) error {
		c.Greeting = "Hello, " + c.FullName
		return nil
	}, "Greeting"),
)
```

This is what makes a field derived from another derived field work, with no separate concept: the second hook sees the first hook's output, because it runs after it, and both `FullName` and `Greeting` are independently reported changed.

## Do not call back into the same Watcher

A derive hook runs on the reconciler goroutine, the same goroutine that services `Pin`, `PinCurrent`, `Unpin`, and `Refresh`. Calling any of those from inside the hook asks that goroutine to answer a command while it is busy being your hook, and mamori refuses rather than hanging: `Pin` and `Refresh` return `ErrReentrantCall`, `PinCurrent` returns `0` (a version that never collides with a real one), and `Unpin` does nothing.

```go
mamori.WithDerive(func(c *Config) error {
	c.DSN = assembleDSN(c)
	// Wrong: asks the reconciler goroutine to service a command while it is
	// busy running this very hook. Refused with ErrReentrantCall rather than
	// hanging, but still a bug: refresh from another goroutine instead, or
	// let the next reconciliation carry the change.
	// _ = w.Refresh(context.Background())
	return nil
}, "DSN"),
```

`Get()` is the exception and always works from inside a derive hook: it is a lock-free read, not a command the reconciler goroutine has to service. A `Pin` from an unrelated goroutine that merely overlaps a running derive hook is unaffected too: it waits and is serviced normally.

## What mamori still cannot see

A hook that writes a field it did not declare in `writes` behaves exactly as it did before declaring existed: mamori cannot inspect the hook's body, so an undeclared write is invisible to `ev.Changed()` and `Status()`, and mamori has no way to detect that it happened at all. Declaring a write does not extend to `mamori explain`, `schema`, or `diff` either way: all three read `source` struct tags statically, a derive has none, and `DSN` appears in none of their output no matter how many fields feed into it. Nor does declaring a write let mamori flag a field carrying both a `source` tag and a derive assignment as a conflict: the derive runs after decoding and its assignment simply wins, with nothing to detect or warn about the overlap.

## See also

- [Rotation safety](/docs/usage/rotation/) - `WithDerive` and `PreApply` together, so a rebuilt value is also a proven one.
- [Options reference](/docs/usage/options/) - `WithDerive` alongside every other option and its default.
- [Watch for changes](/docs/usage/watching/) - `Change`, `Changed`, and `OnChange` in full.
- [Observability](/docs/observability/) - `Status()` and the `Report`/`FieldStatus` shape a declared derived field joins.
