---
layout: ../../../layouts/DocsLayout.astro
title: Derived fields
---

# Derived fields

A DSN you assemble yourself, after `Get()`, is built **once**:

```go
cfg := w.Get()
dsn := fmt.Sprintf("postgres://%s:%s@%s/app", cfg.User, cfg.Pass.Reveal(), cfg.Host)
pool := connect(dsn)
```

Three weeks later the password rotates. `w.Get().Pass` returns the new one immediately, but `dsn` still holds the old one, because nothing ever asked it to rebuild. Your pool keeps using a credential that is about to be revoked, and `w.Status()` reports every field healthy, because mamori reconciled everything it knows about and stopped where you took over.

## The fix

`WithDerive` moves the assembly inside mamori, so it reruns on every update:

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

The hook runs on every `Load` and every reconciled update, after fields are decoded and before validation. `DSN` is rebuilt from whatever `User`, `Pass`, and `Host` hold right now.

Use `net/url` rather than `fmt.Sprintf`. A rotated password containing `@` or `/` silently breaks a `Sprintf`-built DSN, parsing into the wrong host or path. `net/url` escapes it correctly.

## Declare what it writes

`"DSN"`, the trailing argument, is the field path the hook writes. mamori cannot read your function to see what it assigns, so the hook says so itself. Declaring it is what makes the field visible to `ev.Changed()` and `Status()`, and it is the ordinary way to call this option.

## React to it

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
	if ev.Changed("DSN") {
		pool.Rotate(ev.New.DSN.Reveal())
	}
})
```

`ev.Changed("DSN")` is true whenever the rebuilt value differs from the one it replaced, whatever input caused it. Trigger on `DSN` itself; there is no need to also watch `Pass`.

## Worth knowing

- **Use `secret.String` or `secret.Bytes`** for anything embedding a credential, so the rebuilt value stays redacted in `fmt`, JSON, and `slog`. mamori never reports a derived field's value, so this protects your own logs. [`mamori vet`](/docs/cli/vet/) flags a hook that reveals a secret and writes the plaintext into a plain field, the same mistake it already flags on a `source:` tag.
- **An error rejects the whole update.** `Get()` keeps returning the last valid config and `OnError` receives a `*DeriveError`. A config whose derived fields did not build is not one to serve, and half-applying it would leave a rotated credential beside a value still derived from the old one.
- **Multiple hooks run in registration order**, each keeping its own declared writes. A field derived from another derived field works for free, since the second hook sees the first one's output.
- **Do not call back into the same `Watcher`.** A hook runs on the reconciler goroutine, so `Pin` and `Refresh` from inside return `ErrReentrantCall`, `PinCurrent` returns `0`, and `Unpin` does nothing. `Get()` is fine, and a `Pin` from another goroutine is unaffected.

## Where it shows up

`mamori status` and `mamori doctor` give a derived field its own `DERIVED` column:

```bash
$ mamori status --endpoint unix:///run/app-admin.sock
PATH  SCHEME  REF                     VERSION   STALE  LAST_KIND  LAST_ERROR  SENSITIVE  DERIVED
Host  env     env://DB_HOST           3         false  -          -           false      false
Pass  aws-sm  aws-sm://prod/db-pw     3         false  -          -           true       false
DSN                                   a3f9c1e2  false  -          -           true       true
```

The blank `SCHEME` and `REF` say mamori maintains this field but never fetched it from anywhere. `VERSION` is a content hash of the value your hook produced, so it moves the moment the credential rotates, even for a secret nested inside a struct.

[`explain`](/docs/cli/explain/), [`schema`](/docs/cli/schema/), and [`diff`](/docs/cli/diff/) list your derived fields too, with the ref and scheme columns empty, exactly as `status` shows above. [`policy`](/docs/cli/policy/) leaves them out: there is no backend behind a derived field, so there is no permission to grant.

Write the path as a literal string, as `"DSN"` is above. If you build it at runtime, from a variable or a slice, these commands cannot see it, and they say so rather than quietly listing fewer fields than you have.

None of them show the value. For that, ask a running process with `status`. To fail a build on a hook that errors, run the [`Doctor` preflight](/docs/observability/doctor/#derived-fields-are-probed) in CI.

## See also

- [Rotation safety](/docs/usage/rotation/) - `WithDerive` and `PreApply` together, so a rebuilt value is also a proven one.
- [Options reference](/docs/usage/options/) - `WithDerive` alongside every other option and its default.
- [Watch for changes](/docs/usage/watching/) - `Change`, `Changed`, and `OnChange` in full.
- [Observability](/docs/observability/) - the full `Report`/`FieldStatus` shape a derived field joins.
