---
name: mamori
description: Use when writing Go code that loads configuration or secrets, wiring providers (env, files, AWS, Vault, GCP, Azure, Kubernetes, Consul, databases, feature flags), watching config for live changes without a restart, validating config structs, keeping secrets redacted, or using the mamori CLI (explain, schema, policy, vet, doctor, status). Covers github.com/xavidop/mamori.
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
	_ "github.com/xavidop/mamori/providers/aws" // registers aws-sm:// and aws-ps://
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
  `default:` value is exempt - it is used as-is, undecoded.
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
- `mamori vet ./...` - flag secret-bearing sources stored in a plain `string`/`[]byte`. Also works as a `go vet` tool: `go vet -vettool=$(which mamori) ./...`.
- `--secret-schemes=mysecrets` - accepted by `explain`, `schema`, `policy`, `vet`, and `doctor --compare`; adds a custom provider's scheme to the built-in secret-bearing set so every command agrees on what is a secret.
- `mamori doctor --endpoint <ep>` / `mamori status` - probe a running process's admin endpoint; exit codes 0 healthy, 1 unhealthy, 2 admin off, 3 unreachable, 4 auth failed.

## When helping a user

- Suggest `secret.String` for secrets and confirm `mamori vet` would pass.
- Prefer `Watch` when the program is long-running and should pick up rotations;
  `Load` for one-shot / CLI programs.
- For CI, recommend `mamori.Doctor` (library) as a pre-deploy check.
- Point to https://mamorigo.dev/docs for provider auth details and the config
  server (a separate fan-out module, `github.com/xavidop/mamori/server`).
