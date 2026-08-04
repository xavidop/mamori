---
name: mamori
description: Use when writing Go code that loads configuration or secrets, wiring providers (env, files, AWS, Vault, GCP, Azure, Kubernetes, Consul, databases, feature flags), watching config for live changes without a restart, validating config structs, keeping secrets redacted, or using the mamori CLI (explain, schema, policy, diff, vet, doctor, status). Covers github.com/xavidop/mamori.
---

# mamori: typed, validated, watchable config and secrets for Go

mamori loads configuration and secrets into a typed Go struct from a broad
provider ecosystem, then keeps that struct reconciled while the program runs.
Reach for it instead of hand-rolling a config manager with a ticker and a mutex.

Full docs: https://mamorigo.dev/docs . Core module: `github.com/xavidop/mamori`.

## The model in one minute

- Each struct field carries a `source:` tag: a ref to a value in a provider
  (`env:LOG_LEVEL`, `aws-sm://prod/db#password`, `file:///etc/x`).
- A **provider** resolves one scheme. Providers register via a blank import (the
  `database/sql` pattern), so the core module has no cloud-SDK dependencies.
- `Load` resolves and validates once. `Watch` resolves once (fail-fast) then keeps
  the struct reconciled, re-validating and atomically swapping on every change.
- Secrets use `secret.String` / `secret.Bytes`, which redact in logs, `fmt`, and
  JSON; only `.Reveal()` exposes the value.

## Define and load config

```go
import (
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	_ "github.com/xavidop/mamori/providers/aws" // registers aws-sm://, aws-ps://, aws-appconfig://
)

type Config struct {
	LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers    int           `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	DBPassword secret.String `source:"aws-sm://prod/db#password" validate:"required"`
}

cfg, err := mamori.Load[Config](ctx) // *Config, or an error and no partial struct
```

- Use `secret.String` / `secret.Bytes` for anything sensitive, never a plain
  `string` (`mamori vet` flags that).
- `default:` applies only to genuine absence (not-found), never to a real error.
- `optional:"true"` leaves a missing field at its zero value instead of failing.
- `validate:` uses go-playground/validator/v10 syntax and runs on load AND on
  every reconciled update; an invalid update is rejected atomically.
- `flatten:"json|yaml|env"` decodes one payload into a nested struct.

## Ref syntax

| Form | Meaning |
| --- | --- |
| `scheme://path` | resolve this ref |
| `#key` | select one top-level key from a JSON payload |
| `#/a/b/5` | RFC 6901 JSON Pointer, any depth, through objects and arrays |
| `?decode=` | value is encoded: `base64`, `base64url`, `hex`, `gzip`, `trim` |
| `?debounce=` | per-field coalescing window, overriding `WithDebounce` |
| `${VAR}` | interpolated from `WithRefVars`, before the tag is parsed |
| `a,b,c` | precedence chain: first ref that resolves wins |

- Reach a nested field with a pointer fragment rather than restructuring the
  secret or adding plumbing: `source:"aws-sm://prod/db#/credentials/password"`.
- `?decode=` codings apply left to right, outermost wrapper first, so
  `?decode=base64,gzip` is base64-decoded then gunzipped. It runs AFTER `#key`
  selection, so it cannot reach into a payload that only exists once decoded:
  drop the `#key`, decode the whole payload, and use `flatten:"json"`. A bad
  payload is a loud `ErrInvalid`; a `default:` value is exempt and used as-is.
- **`${VAR}` expands only from `WithRefVars`, never `os.Getenv` or any ambient
  source.** A ref decides which secret gets read, so it must not be steerable by
  anything able to set an environment variable. To source one from the
  environment, opt in by name: `mamori.WithRefVars(mamori.EnvVars("ENV", "REGION"))`.
  Never suggest reading `os.Getenv` into a ref. A bare `$VAR` is untouched, `$$`
  is a literal `$`, and an undefined variable or unterminated `${` is a hard
  error. Values must not be secrets: the expanded ref appears in `Status()`, the
  admin `Report`, and CLI output.
- On a chain, `onfail:"keeplast|default|fail"` governs what happens on a real
  error, as opposed to a not-found.

## Watch for live changes

```go
w, err := mamori.Watch[Config](ctx,
	mamori.OnChange(func(ev mamori.Change[Config]) {
		if ev.Changed("DBPassword") {
			pool.Rotate(ev.New.DBPassword.Reveal())
		}
	}),
)
if err != nil { return err }
defer w.Close()

cfg := w.Get() // latest fully-valid snapshot, safe to call anytime
```

`OnChange[T]` must be typed to the same `T` passed to `Watch[T]`/`Load[T]`. A
mismatch fails `Watch`/`Load` outright with an error wrapping `ErrInvalid`
naming both types, rather than compiling clean and never firing.

## Verify a rotated credential before it goes live

`OnChange` fires after `Get()` already serves the new value, too late to refuse a
credential that does not work. `PreApply` runs after validation and before the
swap; returning an error rejects the candidate (`Get()` keeps the last good
config, `OnChange` does not fire, `OnError` gets a `*PreApplyError`). It runs on
the initial load too, so a bad credential fails at startup, not at first rotation.

```go
mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
	if !ev.Changed("DBPassword") {
		return nil // only re-verify the field that actually rotated
	}
	return pool.Ping(ctx, ev.New.DBPassword.Reveal())
})
```

- `WithPreApplyTimeout` bounds the hook (default 10s) and cannot be disabled.
  Exceeding it is a **rejection**: mamori does not know the candidate works.
- **Never call back into the same `Watcher` from `PreApply`, `WithDerive`, or
  `OnError`.** During reconciliation all three run inline on the reconciler
  goroutine, so `Pin` and `Refresh` return `ErrReentrantCall`, `PinCurrent`
  returns `0`, and `Unpin` does nothing. `Get()` is safe. `OnChange` runs on its
  own goroutine and is exempt. (On the initial load they run on the caller's
  goroutine instead, where there is no watcher to call back into yet.)

## Derive fields from already-resolved fields

`WithDerive` computes a field from other resolved fields, so a value assembled
from several of them is rebuilt on every update instead of going stale when one
input rotates. It runs after decode, before validation.

```go
mamori.WithDerive(func(c *Config) error {
	c.DSN = secret.NewString((&url.URL{
		Scheme: "postgres", User: url.UserPassword(c.User, c.Pass.Reveal()),
		Host: c.Host, Path: "/app",
	}).String())
	return nil
}, "DSN")
```

- Assemble with `net/url`, not `fmt.Sprintf`: a password containing `@` or `/`
  needs escaping. Assign into `secret.String` so the result stays redacted.
- The trailing paths declare what the hook writes. Declaring them is what makes
  the field visible to `ev.Changed()` and `Status()`; an undeclared write is
  invisible. Trigger on the derived field, not on each input.
- Returning an error rejects the whole candidate, reaching `OnError` as a
  `*DeriveError`. Multiple hooks run in registration order.
- `explain`, `schema`, and `diff` do see a derived field (no ref, no scheme), by
  reading this call site rather than a `source:` tag. `policy` still grants it
  nothing, since it has no ref.

## Boot through a backend outage

`WithBootstrapCache(path, key, ...)` keeps an encrypted snapshot of the last
known-good **resolved values** on disk and boots from it when a cold start
cannot reach the backend. `key` is exactly 32 bytes (AES-256-GCM); the file is
written atomically, mode `0600`.

```go
mamori.WithBootstrapCache("/var/lib/app/mamori.snap", key,
	mamori.BootstrapMaxAge(6*time.Hour))
```

- **It creates a file holding live credentials at rest that did not exist
  before.** Say so when you recommend it.
- **Fallback, never a fast path.** Every start resolves normally first. The
  snapshot is read only if that fails, and only on `unavailable` or
  `rate_limited`; `not_found`, `permission_denied`, `unauthenticated`, `invalid`
  and `unknown` fail the start, as does a record whose `Value.NotAfter` passed.
- `Health()` passes inside `BootstrapMaxAge` (default 24h) and returns a
  `*BootstrapStaleError` past it. Set it to the rotation window of the
  shortest-lived credential; `0` is unbounded and must be written explicitly.
- Changing the config struct invalidates an older snapshot. Give each replica
  its own path, on a volume that outlives the container.

## Force an immediate refresh

`w.Refresh(ctx)` re-resolves every field now, bypassing poll intervals, and
blocks until the result is applied or rejected, so a SIGHUP handler learns
whether the reload worked.

```go
switch err := w.Refresh(ctx); {
case err == nil:
	log.Println("reload applied") // also returned when nothing changed
case ctx.Err() != nil:
	log.Printf("stopped waiting; the reload still proceeds: %v", err)
default:
	log.Printf("reload rejected: %v", err) // derive, validation, PreApply, onfail:"fail"
}
```

- It still runs `PreApply`. A forced refresh is gated like any other.
- Cancelling `ctx` stops the wait, not the work. There is no half-applied snapshot.
- It occupies the reconciler goroutine, so use it from an operator trigger
  (SIGHUP, an authorized admin route), not a hot path.

## Pin a snapshot

`w.Pin(version)` freezes what `Get()` returns while sources keep being watched
underneath: `Status` reports `Live` advancing while `Snapshot` stays put. Use it
to hold a known-good config during an incident. It returns `ErrNoSuchSnapshot`
if that version is no longer retained, so raise `WithHistory` to pin further
back. `w.PinCurrent()` pins what is being served right now and returns its
version, `w.Unpin()` releases, and `w.Pinned()` reports the state.

`WithHistory(n)` keeps the last `n` snapshots reachable through `w.History()`,
each with its `Version`, `At`, `Config`, and changed `Fields`. It extends the
in-memory lifetime of old secrets, so enable it deliberately.

## Watch one ref without a struct

`mamori.WatchRef(ctx, provider, ref, opts...) <-chan Update` streams changes for
a single ref, for cases that do not warrant a config struct. It uses the
provider's native watch when there is one and polls otherwise.

## Options reference

| Option | Default | Purpose |
| --- | --- | --- |
| `OnChange(fn)` | none | called after an update is applied, on its own goroutine |
| `OnError(fn)` | none | called inline on the reconciler goroutine |
| `PreApply(fn)` | none | gate a candidate before the swap |
| `WithDerive(fn, paths...)` | none | compute fields from resolved fields |
| `WithProvider(p)` | registry | register a provider for this call only |
| `WithValidator(v)` | go-playground | replace the validator |
| `WithDecodeHook(h)` | none | mapstructure hook for `flatten:` payloads |
| `WithRefVars(m)` | none | values for `${VAR}` in refs |
| `WithExecProvider()` | off | opt in to the `exec:` scheme |
| `WithPollInterval(d)` | 30s | poll cadence for providers without native watch |
| `WithJitter(f)` | 0.2 | fraction of the interval to randomize |
| `WithDebounce(d)` | 500ms | coalesce bursts of updates |
| `WithQueueDepth(n)` | 16 | `OnChange` queue; drops oldest when full |
| `WithBackoff(base, max)` | provider default | retry pacing after failures |
| `WithStale(maxAge)` | off | a ref unrefreshed this long sends `OnError` a `*StaleError` |
| `WithBootstrapCache(path, key, ...)` | off | boot from an encrypted on-disk snapshot when a cold start cannot reach the backend |
| `BootstrapMaxAge(d)` | 24h | a `BootstrapOption`: how old a restored snapshot may be while `Health` passes |
| `WithPreApplyTimeout(d)` | 10s | bound the `PreApply` hook |
| `WithHistory(n)` | 0 | keep the last `n` snapshots |
| `WithLogger(l)` | discard | structured trail; never logs a resolved value |
| `WithMeter(m)` | noop | metrics; see `x/otel`, `x/prom` |
| `WithTracer(t)` | noop | spans around resolves |
| `WithClock(c)` | system | inject time in tests |
| `WithAdminHTTP(addr, ...)` | off | serve `Report` over HTTP |
| `WithAdminTLS(cfg)` | off | TLS for the admin endpoint |
| `WithAuth(a)` | none | a `HandlerOption` for `WithAdminHTTP`, not an `Option` |

## Error kinds

`mamori.ErrorKind(err)` classifies a failure as `not_found`,
`permission_denied`, `unauthenticated`, `unavailable`, `rate_limited`,
`invalid`, or `unknown`. `not_found`, `permission_denied`, `unauthenticated`,
and `invalid` are terminal and will not clear without human action;
`unavailable` and `rate_limited` are expected to self-heal.
Sentinels: `ErrNotFound`, `ErrInvalid`, `ErrReentrantCall`,
`ErrNoSuchSnapshot`. Typed errors: `*ProviderError`, `*PreApplyError`,
`*DeriveError`, `*StaleError`.

## Choosing a provider

Pick the scheme, add its module, blank-import it. See `references/providers.md`
for the full list and ref syntax.

- `env:NAME`, `file:///path`, `dotenv://...`, `exec:...` (core, no extra module)
- `aws-sm://` / `aws-ps://`, `vault://`, `gcp-sm://`, `azure-kv://`, `doppler://`,
  `op://` (1Password), `sops://`
- `k8s-secret://` / `k8s-cm://`, `consul://`, `etcd://`, databases
  (`postgres://`, `mysql://`, `redis://`, `mongodb://`, ...), object stores,
  and feature-flag backends.

## The mamori CLI

`brew install xavidop/tap/mamori` or `go install github.com/xavidop/mamori/cmd/mamori@latest`.

- `mamori explain ./...` - list every `source:` ref in a package's config structs.
- `mamori schema ./...` - emit JSON Schema from field types and `validate:` tags.
- `mamori policy ./... --format=aws-iam|gcp|external-secret` - least-privilege artifact.
- `mamori diff <base.json> <head.json>` - compare two `explain --json` outputs:
  fields and chains added or removed, fields newly reading secret material, and
  the privilege delta. Built for PR CI: `--markdown` suits a comment, and
  `--exit-code=privilege` fails only when the permission surface grows.
- `mamori vet ./...` - flag a secret-bearing source, or a `WithDerive` hook that
  reveals one, stored in a plain `string`/`[]byte`. Also works as a `go vet`
  tool: `go vet -vettool=$(which mamori) ./...`.
- `mamori doctor --endpoint <ep>` / `mamori status` - probe a running process's
  admin endpoint; exit 0 healthy, 1 unhealthy, 2 admin off, 3 unreachable, 4 auth failed.
- `--secret-schemes=mysecrets` - accepted by `explain`, `schema`, `policy`,
  `vet`, and `doctor --compare`, so every command agrees on what is a secret.

The static commands read Go source, never a running process. `explain`,
`schema`, and `diff` do see a `WithDerive`-declared field (no ref, no scheme);
`policy` still grants it nothing, since a derive carries no ref to turn into a
permission.

## When helping a user

- Suggest `secret.String` for secrets and confirm `mamori vet` would pass.
- Prefer `Watch` for long-running programs, `Load` for one-shot and CLI programs.
- For CI, recommend the `mamori.Doctor` library call as a pre-deploy check. It
  resolves every field once, reporting all failures rather than the first, and
  runs every `WithDerive` hook, so a hook that errors or is typed for the wrong
  config fails the preflight. A derived row carries a content-hash `Version` when
  the hook ran; it is blank when the hook failed, when an input produced no
  value, or when the hooks could not be typed. Note it executes hook code, so a
  hook with side effects runs for real.
- In a custom `Meter`, `Tracer`, or middleware, use `ref.Redacted()`, never
  `ref.Raw`. A ref can carry an inline credential in a query option, and
  `Redacted()` is the denylist everything inside mamori already applies.
- Suggest `WithLogger(slog.Default())` for a resolve-failure trail, and
  `WithMeter` to make failures alertable. `RecordChangeDropped()` is the signal
  that an `OnChange` handler is too slow to keep up.
- Point to https://mamorigo.dev/docs for provider auth and the config server
  (a separate fan-out module, `github.com/xavidop/mamori/server`).
