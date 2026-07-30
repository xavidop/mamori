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

- Each struct field carries a `source:` tag: a small ref to a value in a
  provider (`env:LOG_LEVEL`, `aws-sm://prod/db#password`, `file:///etc/x`).
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
	_ "github.com/xavidop/mamori/providers/aws" // registers aws-sm://, aws-ps://, and aws-appconfig://
)

type Config struct {
	LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers    int           `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	DBPassword secret.String `source:"aws-sm://prod/db#password" validate:"required"`
}

cfg, err := mamori.Load[Config](ctx) // returns *Config, or an error and no partial struct
```

Rules to hold onto:
- Use `secret.String` / `secret.Bytes` for anything sensitive. Never a plain
  `string` for a secret (`mamori vet` flags that).
- `default:` applies only to genuine absence (not-found), never to a real error.
- `validate:` uses go-playground/validator/v10 syntax and runs on load AND on
  every reconciled update; an invalid update is rejected atomically.
- `#key` selects one field from a JSON payload. A fragment beginning with `/`
  is an RFC 6901 JSON Pointer, addressing a value at any depth through objects
  and array elements: `source:"aws-sm://prod/db#/credentials/password"`,
  `source:"aws-sm://prod/db#/replicas/5/host"`. Any other fragment is a
  literal top-level key, exactly as before: `source:"k8s-secret://prod/tls#ca.crt"`.
  Do not restructure a secret or add application-code plumbing to reach a
  nested field when a pointer fragment already reaches it directly.
- `?decode=` declares a resolved value is encoded, so core decodes it before
  the field is populated: `source:"aws-sm://prod/tls#key?decode=base64"`.
  Codings are `base64`, `base64url`, `hex`, `gzip`, `trim` - a closed,
  stdlib-only set - applied left to right, outermost wrapper first:
  `?decode=base64,gzip` reads as "base64 of gzip", so it is base64-decoded
  then gunzipped. Decode runs *after* `#key` selection, so it cannot reach
  into a payload that only exists once decoded - drop the `#key`, decode the
  whole payload, and use `flatten:"json"` for that case instead. A bad
  payload is a loud `ErrInvalid`, never silently passed through; a field's
  `default:` value is exempt - it is used as-is, undecoded. Decoding is done
  by core in the process that loads the config, so `?decode=` belongs on the
  `source` tag you write - including a `mamori://name` ref pointing at a
  config server. A server-side binding may not carry it (`server.New`
  rejects it), since that server never runs the pipeline.
- `${VAR}` in a `source` tag is ref interpolation: it expands from the map
  passed to `mamori.WithRefVars(map[string]string{...})`, once, before the
  tag is parsed - `source:"aws-sm://${ENV}/db#password"`. **Variables come
  only from `WithRefVars`, never `os.Getenv` or any other ambient source** -
  the same opt-in posture as `exec:`/`WithExecProvider`, because a ref
  decides which secret gets read and that must not be steerable by anything
  able to set an environment variable. If a variable's value should come
  from the environment, opt in explicitly and by name with
  `mamori.WithRefVars(mamori.EnvVars("ENVIRONMENT", "REGION"))` - do not
  suggest reading `os.Getenv` directly into a ref. A bare `$VAR` (no braces)
  is untouched and `$$` is a literal `$`. An undefined variable, an
  unterminated `${`, or an empty `${}` are all hard errors, not a silently
  empty path segment. `WithRefVars` values must not be secrets: the
  expanded ref is visible in `Status()`, the admin `Report`, and
  `mamori doctor` output - use it for environment names, regions, and
  similar identifiers, never for anything confidential.

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

## Verify a rotated credential before it goes live

`OnChange` fires after `Get()` already serves the new value - too late to refuse a credential that turns out not to work. `mamori.PreApply` installs a gate that runs after validation and before the atomic swap; returning an error rejects the candidate (`Get()` keeps the last good config, `OnChange` does not fire, `OnError` gets a `*PreApplyError`) and the check runs on the initial load too, so a bad configured credential fails `Watch`/`Load` at startup rather than at the first rotation.

```go
mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
	if !ev.Changed("DBPassword") {
		return nil // guard: only re-verify the field that actually rotated
	}
	return pool.Ping(ctx, ev.New.DBPassword.Reveal())
})
```

Rules to hold onto:
- `WithPreApplyTimeout` bounds the hook, defaulting to 10s. It cannot be disabled, and exceeding it is a **rejection**, not an acceptance - mamori does not know the candidate works, so it does not apply it.
- The hook runs on the reconciler goroutine. `w.Get()` is lock-free and safe to call from inside it. `w.Pin`, `w.PinCurrent`, `w.Unpin`, and `w.Refresh` are commands serviced by that same goroutine, and the timeout does not rescue them - `Pin`/`PinCurrent`/`Unpin` take no `ctx` at all, and `Refresh` takes the caller's `ctx`, not the hook's. Never call back into the same `Watcher` from a `PreApply` hook. mamori detects it if you do, and refuses the call rather than hanging: `Pin` and `Refresh` return `ErrReentrantCall`, `PinCurrent` returns `0` (versions start at 1), and `Unpin` does nothing and leaves the watcher pinned. Calls from any other goroutine are unaffected.
- **The same rule covers `OnError`,** which is delivered inline on the reconciler goroutine rather than through the `OnChange` queue. Do not call `w.Refresh` from an `OnError` callback to retry a rejected reload - it returns `ErrReentrantCall`. Issue it from another goroutine, or let the next reconciliation carry it. `OnChange` is safe: it runs on its own dispatch goroutine.
- A hook typed for a different config than the one passed to `Watch[T]`/`Load[T]` fails `Watch` and `Load` outright (an error wrapping `ErrInvalid`), rather than silently leaving the gate open.

## Force an immediate refresh

`w.Refresh(ctx)` re-resolves every field right now, bypassing poll intervals, and **blocks until the result is applied or rejected** - which is the point: a SIGHUP handler wants to know whether the reload it triggered actually worked, not just that it was requested.

```go
for range sighupCh {
	switch err := w.Refresh(ctx); {
	case err == nil:
		log.Println("reload applied")
	case ctx.Err() != nil:
		// You stopped waiting; the reload still proceeds. Not a rejection.
		log.Printf("stopped waiting for the reload: %v", err)
	default:
		log.Printf("reload rejected: %v", err) // validation, PreApply, or onfail:"fail"
	}
}
```

- It returns `nil` when a snapshot was applied *and* when nothing had changed; both mean `Get()` is current.
- It still runs `PreApply` - a forced refresh is gated exactly like any other one, never bypassed.
- Cancelling `ctx` stops the wait and returns `ctx.Err()`, but the reconciler still finishes re-resolving and applying (or rejecting) what it already started; there is no half-applied snapshot.
- It runs on the reconciler goroutine, so for its duration `Pin`/`Unpin` wait, watch updates queue up, and no new `Report` is published - reach for it from an operator trigger (SIGHUP, an authorized admin route), not a hot path.
- For a `mamori://` field, `Refresh` re-reads the config server's current value; it does not force that server to re-fetch its own upstream (see the [config server docs](https://mamorigo.dev/docs/server) for why that stays gated).

## Choosing a provider

Pick the scheme, add its module, blank-import it. Common ones (see
`references/providers.md` for the full list and ref syntax):

- `env:NAME`, `file:///path`, `dotenv://...`, `exec:...` (core, no extra module)
- `aws-sm://` / `aws-ps://`, `vault://`, `gcp-sm://`, `azure-kv://`, `doppler://`,
  `op://` (1Password), `sops://`
- `k8s-secret://` / `k8s-cm://`, `consul://`, `etcd://`, databases
  (`postgres://`, `mysql://`, `redis://`, `mongodb://`, ...), object stores,
  and feature-flag backends.

Precedence chains: a `source:` tag may list several refs comma-separated
(`env:PORT,aws-ps://svc/port`); the first that resolves wins, and `onfail`
(keeplast / default / fail) governs what happens on a real error.

## The mamori CLI

`brew install xavidop/tap/mamori` or `go install github.com/xavidop/mamori/cmd/mamori@latest`.

- `mamori explain ./...` - list every `source:` ref in a package's config structs.
- `mamori schema ./...` - emit JSON Schema from field types and `validate:` tags.
- `mamori policy ./... --format=aws-iam|gcp|external-secret` - least-privilege access artifact.
- `mamori diff <base.json> <head.json>` - compare two `mamori explain --json` outputs: fields and chains added, removed, or modified, fields that newly read secret material, and the privilege delta (backend paths gained and lost). Built for pull request CI: `--markdown` suits a PR comment, and `--exit-code=privilege` fails the build only when the permission surface grows.
- `mamori vet ./...` - flag secret-bearing sources stored in a plain `string`/`[]byte`. Also works as a `go vet` tool: `go vet -vettool=$(which mamori) ./...`.
- `--secret-schemes=mysecrets` - accepted by `explain`, `schema`, `policy`, `vet`, and `doctor --compare`; adds a custom provider's scheme to the built-in secret-bearing set so every command agrees on what is a secret.
- `mamori doctor --endpoint <ep>` / `mamori status` - probe a running process's admin endpoint; exit codes 0 healthy, 1 unhealthy, 2 admin off, 3 unreachable, 4 auth failed.

## When helping a user

- Suggest `secret.String` for secrets and confirm `mamori vet` would pass.
- Prefer `Watch` when the program is long-running and should pick up rotations;
  `Load` for one-shot / CLI programs.
- For CI, recommend `mamori.Doctor` (library) as a pre-deploy check.
- Suggest `mamori.WithLogger(slog.Default())` for a structured log trail of
  resolve failures, watch errors, and applied changes - it never logs a
  resolved value and is silent (discard logger) until opted in.
- Suggest `mamori.WithMeter` (see `x/otel` for an OpenTelemetry adapter, or
  `x/prom` for a direct `prometheus/client_golang` implementation if the shop
  has not adopted OpenTelemetry) to make failures alertable, not just
  loggable - `RecordChangeDropped()` in particular is the signal that an
  `OnChange` handler is too slow to keep up.
- Point to https://mamorigo.dev/docs for provider auth details and the config
  server (a separate fan-out module, `github.com/xavidop/mamori/server`).
