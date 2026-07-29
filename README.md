<div align="center">

# 守り &nbsp;mamori

### Typed, watchable config & secrets for Go

*Load configuration and secrets from anywhere into validated Go structs - and keep them reconciled at runtime, without a restart.*

[![Go Reference](https://pkg.go.dev/badge/github.com/xavidop/mamori.svg)](https://pkg.go.dev/github.com/xavidop/mamori)
[![CI](https://github.com/xavidop/mamori/actions/workflows/ci.yml/badge.svg)](https://github.com/xavidop/mamori/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/xavidop/mamori)](https://goreportcard.com/report/github.com/xavidop/mamori)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

</div>

---

`mamori` (守り - Japanese for *protection / safeguard*) is an embedded Go library that loads application configuration and secrets from heterogeneous sources - environment, files, AWS Secrets Manager, Vault, GCP, Azure, Kubernetes, Consul, and more - into **typed, validated structs**, and keeps them **reconciled at runtime**. When a source value changes, `mamori` detects it, re-validates the whole configuration, and - only if the new snapshot is valid - atomically swaps it in and hands your application a diff-aware callback so it can react (rotate a DB pool, rebuild a client) *without restarting*.

> Think: External Secrets Operator's provider model, one layer down - as a library **inside your process** instead of an operator inside your cluster.

## Why

The primitives exist, but nobody composed them. `runtimevar` watches one variable but has no struct composition or validation. Viper/koanf do multi-source config but treat secrets and rotation as afterthoughts. The AWS caching client and Vault's `LifetimeWatcher` refresh one provider each, in silos. So every production Go service hand-rolls a `ConfigManager` with a ticker, a mutex, and a prayer. `mamori` is that glue, done once, with a provider ecosystem and a conformance kit.

## Install

```bash
go get github.com/xavidop/mamori
```

`env:` and `file://` work out of the box. Cloud providers are separate modules so the core has **zero cloud-SDK dependencies**:

```bash
go get github.com/xavidop/mamori/providers/aws     # aws-sm://  aws-ps://  aws-appconfig://
go get github.com/xavidop/mamori/providers/vault   # vault://
go get github.com/xavidop/mamori/providers/k8s     # k8s-secret://  k8s-cm://
# ... gcp, azure, consul, doppler, onepassword, sops
```

## Quick start

```go
type Config struct {
    // A secret string from AWS Secrets Manager (redacted in logs by default).
    // ${ENV} is ref interpolation: expanded from WithRefVars below, never
    // from the ambient environment.
    DBPassword secret.String `source:"aws-sm://${ENV}/db#password"`

    // Plain config from the environment, with a default and validation
    LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
    Workers    int           `source:"env:WORKERS"   default:"4"    validate:"gte=1,lte=256"`

    // A precedence chain: an environment override wins if set, otherwise a
    // centrally managed Parameter Store value, otherwise the default.
    Port       string        `source:"env:PORT,aws-ps://svc/port" default:"8080"`

    // A nested field, selected with an RFC 6901 JSON Pointer fragment
    DBUser     string        `source:"aws-sm://prod/db#/credentials/user"`

    // A file-backed value, hot-reloaded via fsnotify
    TLSCert    []byte        `source:"file:///etc/tls/tls.crt"`

    // ?decode=base64 declares the stored value is base64; core decodes it
    // back to raw bytes before TLSKey is populated
    TLSKey     []byte        `source:"aws-sm://prod/tls#key?decode=base64"`

    // A nested struct decoded from one JSON secret
    Redis      RedisConfig   `source:"aws-sm://prod/redis" flatten:"json"`
}

// One-shot load
cfg, err := mamori.Load[Config](ctx)

// Or: watch and reconcile at runtime
w, err := mamori.Watch[Config](ctx,
    // Expands DBPassword's ${ENV} above - see "Ref interpolation" below.
    mamori.WithRefVars(map[string]string{"ENV": "prod"}),
    // Proves a rotated password actually opens a connection *before* it
    // becomes what Get() serves - see "Rotation-safe" below.
    mamori.PreApply(func(ctx context.Context, ev mamori.Change[Config]) error {
        if !ev.Changed("DBPassword") {
            return nil
        }
        return pool.Ping(ctx, ev.New.DBPassword.Reveal())
    }),
    mamori.OnChange(func(ev mamori.Change[Config]) {
        if ev.Changed("DBPassword") {
            pool.Rotate(ev.New.DBPassword.Reveal())
        }
    }),
    mamori.OnError(func(err error) { metrics.Inc("config_error") }),
)
defer w.Close()

cfg := w.Get() // lock-free snapshot; always the last *valid* config
```

## What makes it different

- **Typed & tag-driven** - one struct, multiple sources, generics API (`Load[T]` / `Watch[T]`).
- **Nested selection** - `#/credentials/password` is an RFC 6901 JSON Pointer, addressing a value at any depth through objects and array elements; any other fragment (`#ca.crt`, `#tls.key`) stays a literal top-level key, exactly as before.
- **Value decoding** - `?decode=base64,gzip` runs a stdlib-only pipeline (`base64`, `base64url`, `hex`, `gzip`, `trim`) over a resolved value before it reaches your struct field, left to right, outermost wrapper first; a bad payload is a loud `ErrInvalid`, never a silent passthrough.
- **Ref interpolation** - `${VAR}` in a `source` tag expands from `mamori.WithRefVars` before the tag is parsed, so a variable can supply a scheme, path, fragment, or query value. Variables come only from `WithRefVars`, never the ambient environment - the same opt-in posture as `exec:` - and an undefined or malformed `${VAR}` is a hard error rather than a silently empty path segment.
- **Precedence chains** - `source:"env:PORT,aws-ps://svc/port"` tries sources in priority order: the first to yield a value wins, not-found falls through to the next, and a real error stops the walk and applies the field's `onfail` policy instead of silently sliding to a lower-priority source. Every position is watched, so precedence is live.
- **Reconciled at runtime** - native watch where the backend supports it (Kubernetes informers, Consul blocking queries, fsnotify), polling with jitter everywhere else, and lease-aware pre-expiry refresh for Vault.
- **Atomic & validated** - an update that fails validation is *rejected*; `Get()` keeps returning the last good config. Config never enters a broken state mid-flight.
- **Rotation-safe** - `PreApply` gates a candidate snapshot right before the atomic swap, so an application can prove a rotated credential actually *works* (a password opens a connection, a token is accepted by its issuer) rather than discovering it is broken in the request path. A rejection keeps serving the last good config and delivers a `*PreApplyError` to `OnError`; the same gate runs on the very first load, so a bad configured credential fails at startup instead of the first rotation.
- **Forceable** - `w.Refresh(ctx)` re-resolves every field right now, bypassing poll intervals, and blocks until the result is applied or rejected (through the same `PreApply` gate, never bypassing it), so a SIGHUP handler or your own admin route learns whether the reload actually worked.
- **Coalesced** - bursts of field changes within a debounce window produce a single `Change` event.
- **Pinnable** - `WithHistory(n)` retains recent snapshots (`w.History()`); `w.PinCurrent()` / `w.Pin(version)` freeze `Get()` at one of them while you debug production, then `w.Unpin()` resumes and fires one coalesced `Change` for everything that changed in the meantime.
- **Secret hygiene by default** - `secret.String` / `secret.Bytes` redact themselves in `String()`, `fmt`, `MarshalJSON`, and `slog`. Only the explicit, greppable `Reveal()` exposes the value. A shipped analyzer (run it as `mamori vet ./...`, or as a `go vet` tool with `go vet -vettool=$(which mamori)`) flags sensitive refs assigned to plain `string` fields.
- **Pluggable** - providers register with the `database/sql` pattern; a `providertest` conformance kit guarantees they all behave identically.
- **Observable** - `w.Status()` reports live per-field health, `w.Health()` backs a Kubernetes readiness probe, and `mamori.Doctor[T]` checks every ref is reachable before you ever deploy.
- **Testable** - the [`mamoritest`](mamoritest/) package gives application code a scriptable in-memory provider (`Set`/`Del`/`Fail`) plus deterministic wait helpers (`WaitForSnapshot`, `WaitForError`), so an `OnChange` handler or error path can be tested without a real backend.

## Providers

| Module | Schemes | Watch | Errors classified beyond not-found |
|---|---|---|---|
| core (built-in) | `env:` · `dotenv://` · `file://` · `exec:` (opt-in) | fsnotify (file/dotenv) · poll (env/exec) | ✅ |
| `providers/aws` | `aws-sm://` · `aws-ps://` · `aws-appconfig://` | poll | ✅ |
| `providers/gcp` | `gcp-sm://` | poll | ✅ |
| `providers/azure` | `azure-kv://` · `azure-appconfig://` | poll | ✅ |
| `providers/vault` | `vault://` | lease-aware poll (`NotAfter`) | ✅ |
| `providers/k8s` | `k8s-secret://` · `k8s-cm://` | **native** (watch API) | ✅ |
| `providers/consul` | `consul://` | **native** (blocking queries) | ✅ |
| `providers/doppler` | `doppler://` | poll | ✅ |
| `providers/onepassword` | `op://` | poll | ✅ |
| `providers/sops` | `sops://` | fsnotify | ✅ |
| `providers/postgres` | `postgres://` | **native** (LISTEN/NOTIFY) | ✅ |
| `providers/mysql` | `mysql://` | poll | ✅ |
| `providers/sqlite` | `sqlite://` | fsnotify | ✅ |
| `providers/mongodb` | `mongodb://` | **native** (change streams) | ✅ |
| `providers/dynamodb` | `dynamodb://` | poll | ✅ |
| `providers/redis` | `redis://` | **native** (keyspace notifications) | ✅ |
| `providers/etcd` | `etcd://` | **native** (watch API) | ✅ |
| `providers/firestore` | `firestore://` | **native** (snapshot listeners) | ✅ |
| `providers/firebase-rc` | `firebase-rc://` | poll | ✅ |
| `providers/firebase-rtdb` | `firebase-rtdb://` | **native** (streaming) | no (chain preserved) |
| `providers/s3` | `s3://` | poll (ETag) | ✅ |
| `providers/gcs` | `gcs://` | poll (generation) | ✅ |
| `providers/azblob` | `azblob://` | poll (ETag) | ✅ |
| `providers/cosmos` | `cosmos://` | poll (ETag) | ✅ |
| `providers/launchdarkly` | `launchdarkly://` | **native** (streaming) | ✅ |
| `providers/unleash` | `unleash://` | poll | n/a (no error surface) |
| `providers/flagsmith` | `flagsmith://` | poll | no (chain preserved) |
| `providers/configcat` | `configcat://` | poll | n/a (no error surface) |
| `providers/split` | `split://` | poll | n/a (no error surface) |
| `providers/growthbook` | `growthbook://` | poll | no (chain preserved) |
| `providers/flipt` | `flipt://` | poll | ✅ |
| `providers/goff` | `goff://` (GO Feature Flag) | poll | ✅ |
| `providers/mamori` | `mamori://` ([config server](#config-server) client) | **native** (SSE stream) | ✅ |

Every provider that passes the [`providertest`](providertest/) conformance kit earns a badge. See each module's README for auth and ref grammar.

The error-classification sweep is complete: every one of the 36 providers now falls into exactly one of three honest states, and `not_found` itself is detected by every provider regardless of which one.

- **✅ classifies** - the provider maps real backend errors onto `mamori.ErrorKind` values beyond `not_found` (`permission_denied`, `unauthenticated`, `unavailable`, `rate_limited`, `invalid`, as the backend's own vocabulary supports): thirty providers across twenty-seven module rows, since core's single row covers four built-in providers (`env:`, `dotenv://`, `file://`, `exec:`).
- **no (chain preserved)** - `providers/firebase-rtdb`, `providers/growthbook`, and `providers/flagsmith` have no backend-specific error vocabulary to map, so a non-not-found failure still reports `unknown`. Their `Resolve` wraps the underlying error with `%w` rather than flattening it, so `errors.Is`/`errors.As` and any mamori sentinel injected by a caller's own middleware still reach it - the chain is preserved even though nothing here narrows it to a more specific kind. Do not read this as classifying permission or availability errors these providers cannot see.
- **n/a (no error surface)** - `providers/unleash`, `providers/configcat`, and `providers/split` wrap SDK client surfaces that return only `bool`/`string` values, with no per-key error at all; their `Resolve` can only ever produce `mamori.ErrNotFound` or a client-construction error, so there is nothing to classify or preserve a chain for. Each is explicitly exempted from the `providertest` conformance kit's `ErrorClassification` case via `providertest.Config.NoResolveErrors`, a deliberate, greppable declaration rather than a silent gap.

## Middleware

Providers compose because they share one interface:

```go
mamori.WithProvider(
    middleware.Cache(5*time.Minute,
        middleware.Audit(logger,
            middleware.Failover(primary, replica))))
```

`Cache`, `Audit`, `Failover`, `RateLimit`, and `Prefix` (multi-tenant namespace rewriting) ship in [`middleware/`](middleware/).

## Observability

`w.Status()` returns a lock-free, point-in-time `Report` of every field's health (ref, staleness, last error kind), safe to log or serialize since values never appear and refs have sensitive query options redacted. `w.Health()` reduces that to a single readiness check: nil when every field is fresh and none carries a terminal error kind (`not_found`, `permission_denied`, `unauthenticated`, `invalid`), a `*HealthError` otherwise - a transient kind like `unavailable` or `rate_limited` only fails health once the field is also stale.

For a pre-deploy check, `mamori.Doctor[Config](ctx, opts...)` resolves every field once without starting a watcher and reports every failure at once, not just the first - run it as a build-tagged CI test to catch a rotated-away secret or a typo'd ref before it ships. An optional HTTP endpoint - `mamori.Handler` on your own mux, or a self-hosted server via `mamori.WithAdminHTTP` - serves that same `Report` as JSON, metadata only and never a configuration value, with a pluggable `Authenticator` (`WithAuth`; basic auth, bearer token, API key, mTLS, or your own) gating access and support for live credential rotation. See [Observability](https://mamorigo.dev/docs/observability) and [Auth](https://mamorigo.dev/docs/auth) for the full picture, including the readiness-probe pattern, the `Doctor` CI test, and credential rotation.

## Config server

[`server/`](server/) is a separate module: a standalone process that fronts a fixed, operator-declared table of name-to-ref bindings (`server.Bind`/`server.BindFile` - never a client-supplied ref) and serves resolved values to authenticated, authorized callers over Unix sockets and TLS TCP, under a mandatory `Policy` and `Authenticator`. It reuses the same `Authenticator`/`Identity` as the admin endpoint above, plus a Unix-socket-only `PeerCred` scheme authenticated by kernel-verified uid/gid. It is deliberately the highest-blast-radius component in this project - it concentrates every backend credential its bindings touch into one process - so read [the docs](https://mamorigo.dev/docs/server) before deploying one, not just the quick start.

## CLI

[`cmd/mamori`](cmd/mamori/) is a standalone CLI with two halves that never mix: `explain`/`schema`/`policy` statically read your Go source and never resolve anything (struct field tables, JSON Schema, least-privilege IAM/GCP/ExternalSecret artifacts), while `doctor`/`status` are thin clients of a running process's admin endpoint (`WithAdminHTTP` above), exiting `0`-`4` so a script can tell a broken config apart from one it merely couldn't reach.

```bash
brew install xavidop/tap/mamori
# or
go install github.com/xavidop/mamori/cmd/mamori@latest
```

See [mamorigo.dev/docs/cli](https://mamorigo.dev/docs/cli) for the full command reference.

## Agent skill

Teach your AI coding agent (Claude Code, Cursor, Copilot, and others) how to use mamori with the shipped [Agent Skill](skills/mamori/):

```bash
npx skills add xavidop/mamori
```

See [mamorigo.dev/docs/skill](https://mamorigo.dev/docs/skill) for what it covers and manual install.

## Documentation

- 📖 **Docs site:** https://mamorigo.dev
- 📦 **API reference:** https://pkg.go.dev/github.com/xavidop/mamori
- 🧩 **Write a provider:** [mamorigo.dev/docs/writing-a-provider](https://mamorigo.dev/docs/writing-a-provider)
- 🏃 **Runnable example:** [examples/basic](examples/basic)

## Project layout

This is a multi-module monorepo. The core (`github.com/xavidop/mamori`) depends only on `go-playground/validator`, `go-viper/mapstructure`, and `fsnotify`. Each provider is its own module with its own release cadence, so a cloud SDK never leaks into your build unless you use that provider.

## Contributing

Contributions welcome - new providers especially. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Write a provider guide](https://mamorigo.dev/docs/writing-a-provider). A provider that passes `providertest` and the conformance kit gets listed here.

## License

[MIT](LICENSE) © Xavier Portilla Edo
