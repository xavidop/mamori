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

`WithDerive` moves that assembly inside mamori, so it happens again on every applied update instead of once.

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithDerive(func(c *Config) error {
		c.DSN = secret.NewString(fmt.Sprintf(
			"postgres://%s:%s@%s/app", c.User, c.Pass.Reveal(), c.Host))
		return nil
	}),
)
```

`WithDerive` installs a hook that runs on every `Load` and every reconciled update, after fields are decoded and before validation, so `DSN` above is rebuilt from whatever `User`, `Pass`, and `Host` currently hold, not from what they held when the process started.

Escaping and secret hygiene are yours, deliberately: `net/url` already escapes a password containing `@` or `/` correctly, and assigning into a `secret.String` (not a plain `string`) is what keeps the assembled value redacted in `fmt`, JSON, and `slog`. A tag-based derivation would have had to reinvent both, so `WithDerive` takes a plain function instead.

Unlike `PreApply`, the hook takes no `context.Context`. `PreApply` does I/O to prove a credential works; a derive is a pure transformation of a struct that has already been resolved, and the missing parameter is how the API says so. If assembling your value needs a network round trip, it is not a derive.

## Multiple hooks run in registration order

`WithDerive` can be given more than once. Each call appends a hook rather than replacing the last one, and they run in the order you registered them:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithDerive(func(c *Config) error {
		c.FullName = c.First + " " + c.Last
		return nil
	}),
	mamori.WithDerive(func(c *Config) error {
		c.Greeting = "Hello, " + c.FullName
		return nil
	}),
)
```

This is what makes a field derived from another derived field work, with no separate concept: the second hook sees the first hook's output, because it runs after it. A slice of hooks rather than one big closure also keeps unrelated derivations in separate functions instead of accreting into a single one that does everything.

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

A hook whose type parameter does not match `Watch[T]`'s fails `Watch` or `Load` outright, with an error wrapping `ErrInvalid` that names both types, the same way a mismatched `PreApply` does. This is deliberately unlike `OnChange`, which silently discards a mismatched callback and simply never calls it: a `WithDerive` hook installed for the wrong type is a caller bug, and left running as a silent no-op it would look exactly like "my derived field is empty and nothing told me why."

## What mamori cannot see

A derive is opaque Go, not a declaration mamori can inspect, and that costs three things.

**`mamori explain`, `schema`, and `diff` never see a derived field.** All three read `source` struct tags from your config type; a derived field carries no tag, so there is nothing for them to find. `DSN` in the example above appears in none of their output, however many fields feed into it.

**A derived field never appears in `Status()`'s per-field report.** Every entry in `Status().Fields` corresponds to a `source`-tagged field mamori resolves and tracks a ref, a version, and a last-error for. A derived field has none of those, so it gets no entry at all. An operator debugging a wrong DSN in production will look at `Status()` first, and find nothing wrong there, because there is nothing there to be wrong: the field simply is not reported.

**A field carrying both a `source` tag and a derive assignment cannot be flagged as a conflict.** mamori decodes the `source` value into the field first and runs derives afterward, so the derive's assignment silently wins; nothing detects or warns about the overlap, because mamori never sees the derive's body, only its return value.

## A derived field never appears in `ev.Changed()`

This is the one limitation most likely to bite, so it gets called out on its own rather than folded into the list above.

Both places mamori computes a diff (the reconciler's candidate build and the coalesced diff `Unpin` produces) walk the same field list `ev.Changed` consults, and that list is populated only from `source`-tagged fields, each compared by its own resolved version. A derived field has neither a `source` tag nor a version: there is nothing to diff, so it can never be in that list, regardless of where in the derive chain it was built or how much it actually changed.

So triggering on the derived field itself does not work:

```go
// Wrong: DSN is derived, so this branch never runs, no matter how often DSN changes.
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("DSN") {
		pool.Rotate(ev.New.DSN.Reveal())
	}
})
```

Trigger on an **input** to the derive instead, and read the derived field once you know it fired:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("Pass") || ev.Changed("Host") || ev.Changed("User") {
		pool.Rotate(w.Get().DSN.Reveal())   // always correct, always rebuilt
	}
})
```

The derived value itself is never stale: `WithDerive` rebuilds it on every applied update whether or not anything reads `ev.Changed` for it. Only the *trigger* has to name the inputs by hand, and forgetting one there means the reaction never fires for that field, even though the derived value the callback would have read is already correct. That bookkeeping is exactly what `WithDerive` set out to delete for the derived field's own assembly; it comes back here, one level up, for whoever reacts to it. It is a real cost of this feature, not a footnote.

## See also

- [Rotation safety](/docs/usage/rotation/) - `WithDerive` and `PreApply` together, so a rebuilt value is also a proven one.
- [Options reference](/docs/usage/options/) - `WithDerive` alongside every other option and its default.
- [Watch for changes](/docs/usage/watching/) - `Change`, `Changed`, and `OnChange` in full.
