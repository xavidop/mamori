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
go get github.com/xavidop/mamori/providers/aws            # aws-sm://  aws-ps://  aws-appconfig://
go get github.com/xavidop/mamori/providers/vault          # vault://
go get github.com/xavidop/mamori/providers/k8s            # k8s-secret://  k8s-cm://
go get github.com/xavidop/mamori/providers/vercel-gc      # vercel-gc://
go get github.com/xavidop/mamori/providers/cloudflare-kv  # cloudflare-kv://
go get github.com/xavidop/mamori/providers/https          # https:// (generic REST)
go get github.com/xavidop/mamori/providers/infisical      # infisical://
go get github.com/xavidop/mamori/providers/scaleway-sm    # scaleway-sm://
# ... gcp, azure, consul, doppler, onepassword, sops

go get github.com/xavidop/mamori/providers/httpcore       # no scheme: the shared HTTP core
```

[`providers/httpcore`](providers/httpcore/) is a library rather than a provider:
it registers no scheme and you never blank-import it. It is what you build a
REST-backed provider **on** - request building, authenticators, status
classification, conditional GET, and a bounded, always-drained response body,
with no dependency outside the standard library. See
[Write a provider: HTTP core](https://mamorigo.dev/docs/writing-a-provider/httpcore).

## Quick start

```go
type Config struct {
    // A secret from AWS Secrets Manager, redacted in logs by default.
    // ${ENV} expands from WithRefVars below, never from the ambient environment.
    DBPassword secret.String `source:"aws-sm://${ENV}/db#password"`

    // Plain config, with a default and validation
    LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
    Workers    int           `source:"env:WORKERS"   default:"4"    validate:"gte=1,lte=256"`

    // A precedence chain: env wins if set, else Parameter Store, else the default
    Port       string        `source:"env:PORT,aws-ps://svc/port" default:"8080"`

    // A nested value, selected with an RFC 6901 JSON Pointer fragment
    DBUser     string        `source:"aws-sm://prod/db#/credentials/user"`

    // A file, hot-reloaded via fsnotify
    TLSCert    []byte        `source:"file:///etc/tls/tls.crt"`

    // ?decode= runs a stdlib decode pipeline before the field is populated
    TLSKey     []byte        `source:"aws-sm://prod/tls#key?decode=base64"`

    // A nested struct decoded from one JSON secret
    Redis      RedisConfig   `source:"aws-sm://prod/redis" flatten:"json"`
}

// One-shot load
cfg, err := mamori.Load[Config](ctx)

// Or: watch and reconcile at runtime
w, err := mamori.Watch[Config](ctx,
    mamori.WithRefVars(map[string]string{"ENV": "prod"}),
    // Prove a rotated password actually works before it becomes what Get() serves
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

- **Typed & tag-driven** - one struct, many sources, generics API (`Load[T]` / `Watch[T]`).
- **Atomic & validated** - an update that fails validation is rejected; `Get()` keeps serving the last good config.
- **[Rotation-safe](https://mamorigo.dev/docs/usage/rotation)** - `PreApply` proves a rotated credential actually works *before* it goes live, at startup and on every rotation.
- **[Derived fields](https://mamorigo.dev/docs/usage/derived-fields)** - `WithDerive` rebuilds a value assembled from several fields, like a DSN from a host, a user, and a password, on every applied update, so it never goes stale after just one input rotates.
- **[Precedence chains](https://mamorigo.dev/docs/concepts/source-chains)** - `source:"env:PORT,aws-ps://svc/port"` tries sources in order, and every position stays watched.
- **[Rich ref grammar](https://mamorigo.dev/docs/concepts/ref-grammar)** - RFC 6901 JSON Pointer selection, `?decode=` pipelines, and `${VAR}` interpolation from an explicit, non-ambient source.
- **Reconciled at runtime** - native watch where the backend supports it, polling with jitter everywhere else, lease-aware refresh for Vault.
- **[Secret hygiene by default](https://mamorigo.dev/docs/concepts/secret-types)** - `secret.String` / `secret.Bytes` redact in `fmt`, JSON, and `slog`; only the greppable `Reveal()` exposes a value, and `mamori vet` flags the ones you missed.
- **[Observable](https://mamorigo.dev/docs/observability)** - live per-field health, a readiness probe, a pre-deploy `Doctor` check, structured logs, and metrics.
- **Pluggable** - providers register with the `database/sql` pattern, and a [`providertest`](providertest/) conformance kit keeps them behaving identically.
- **[Testable](mamoritest/)** - a scriptable in-memory provider plus deterministic wait helpers, so `OnChange` and error paths are testable without a real backend.

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
| `providers/infisical` | `infisical://` | poll | ✅ |
| `providers/scaleway-sm` | `scaleway-sm://` | poll | ✅ |
| `providers/onepassword` | `op://` | poll | ✅ |
| `providers/sops` | `sops://` | fsnotify | ✅ |
| `providers/postgres` | `postgres://` | **native** (LISTEN/NOTIFY) | ✅ |
| `providers/mysql` | `mysql://` | poll | ✅ |
| `providers/sqlite` | `sqlite://` | fsnotify | ✅ |
| `providers/mongodb` | `mongodb://` | **native** (change streams) | ✅ |
| `providers/dynamodb` | `dynamodb://` | poll | ✅ |
| `providers/redis` | `redis://` | **native** (keyspace notifications) | ✅ |
| `providers/etcd` | `etcd://` | **native** (watch API) | ✅ |
| `providers/vercel-gc` | `vercel-gc://` | poll (digest) | ✅ |
| `providers/cloudflare-kv` | `cloudflare-kv://` | poll | ✅ |
| `providers/https` | `https://` (generic, operator-declared endpoints) | poll (conditional GET) | ✅ |
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
| `providers/posthog` | `posthog://` | poll | ✅ |
| `providers/openfeature` | `openfeature://` ([OpenFeature](https://openfeature.dev) standard) | poll | ✅ |
| `providers/viper` | `viper://` ([Viper](https://github.com/spf13/viper) config library) | poll | n/a (no error surface) |
| `providers/mamori` | `mamori://` ([config server](#config-server) client) | **native** (SSE stream) | ✅ |

`not_found` is detected by every provider. The last column says whether a provider also maps *other* backend failures onto a `mamori.ErrorKind`; the two non-✅ states are honest declarations, not gaps. See [Providers](https://mamorigo.dev/docs/providers) for what each state means, and each module's README for auth and ref grammar.

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

`w.Status()` reports every field's health, `w.Health()` backs a readiness probe, and `mamori.Doctor[Config]` checks every ref resolves before you deploy. `mamori.Handler` or `WithAdminHTTP` serves that same report as JSON behind a pluggable `Authenticator`. Metadata only: a configuration value never appears in a report, a log line, or the admin endpoint.

`WithLogger` (structured logs), `WithMeter` (metrics, with [`x/otel`](x/otel/) and [`x/prom`](x/prom/) bridges), and `WithTracer` are all silent no-ops until you opt in.

See [Observability](https://mamorigo.dev/docs/observability), [Telemetry](https://mamorigo.dev/docs/telemetry), and [Auth](https://mamorigo.dev/docs/auth).

## Config server

[`server/`](server/) is a separate module: a standalone process that serves resolved values from a fixed, operator-declared table of bindings (never a client-supplied ref) over Unix sockets and TLS TCP, under a mandatory `Policy` and `Authenticator`.

It is deliberately the highest-blast-radius component in this project - it concentrates every backend credential its bindings touch into one process - so read [the docs](https://mamorigo.dev/docs/server) before deploying one, not just the quick start.

## CLI

[`cmd/mamori`](cmd/mamori/) has two halves that never mix: `explain`/`schema`/`policy`/`diff` never resolve anything, while `doctor`/`status` are thin clients of a running process's admin endpoint, exiting `0`-`4` so a script can tell a broken config from an unreachable one.

`mamori diff` compares two `explain --json` outputs and reports the **privilege delta**: which backend paths a change starts and stops reading, optionally as concrete IAM or GCP grants. `--exit-code=privilege` makes it a merge gate that fires only when the permission surface grows.

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
- 🚀 **Quick start:** [mamorigo.dev/docs/quickstart](https://mamorigo.dev/docs/quickstart)
- ⚙️ **Options reference:** [mamorigo.dev/docs/usage/options](https://mamorigo.dev/docs/usage/options)
- 🩺 **Troubleshooting:** [mamorigo.dev/docs/troubleshooting](https://mamorigo.dev/docs/troubleshooting)
- 🐍 **Coming from Viper?** [mamorigo.dev/docs/providers/viper](https://mamorigo.dev/docs/providers/viper)
- 📦 **API reference:** https://pkg.go.dev/github.com/xavidop/mamori
- 🧩 **Write a provider:** [mamorigo.dev/docs/writing-a-provider](https://mamorigo.dev/docs/writing-a-provider)
- 🏃 **Runnable example:** [examples/basic](examples/basic)

## Project layout

This is a multi-module monorepo. The core (`github.com/xavidop/mamori`) depends only on `go-playground/validator`, `go-viper/mapstructure`, and `fsnotify`. Each provider is its own module with its own release cadence, so a cloud SDK never leaks into your build unless you use that provider.

## Contributing

Contributions welcome - new providers especially. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Write a provider guide](https://mamorigo.dev/docs/writing-a-provider). A provider that passes `providertest` and the conformance kit gets listed here.

## License

[MIT](LICENSE) © Xavier Portilla Edo
