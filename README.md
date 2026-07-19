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
go get github.com/xavidop/mamori/providers/aws     # aws-sm://  aws-ps://
go get github.com/xavidop/mamori/providers/vault   # vault://
go get github.com/xavidop/mamori/providers/k8s     # k8s-secret://  k8s-cm://
# ... gcp, azure, consul, doppler, onepassword, sops
```

## Quick start

```go
type Config struct {
    // A secret string from AWS Secrets Manager (redacted in logs by default)
    DBPassword secret.String `source:"aws-sm://prod/db#password"`

    // Plain config from the environment, with a default and validation
    LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
    Workers    int           `source:"env:WORKERS"   default:"4"    validate:"gte=1,lte=256"`

    // A file-backed value, hot-reloaded via fsnotify
    TLSCert    []byte        `source:"file:///etc/tls/tls.crt"`

    // A nested struct decoded from one JSON secret
    Redis      RedisConfig   `source:"aws-sm://prod/redis" flatten:"json"`
}

// One-shot load
cfg, err := mamori.Load[Config](ctx)

// Or: watch and reconcile at runtime
w, err := mamori.Watch[Config](ctx,
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
- **Reconciled at runtime** - native watch where the backend supports it (Kubernetes informers, Consul blocking queries, fsnotify), polling with jitter everywhere else, and lease-aware pre-expiry refresh for Vault.
- **Atomic & validated** - an update that fails validation is *rejected*; `Get()` keeps returning the last good config. Config never enters a broken state mid-flight.
- **Coalesced** - bursts of field changes within a debounce window produce a single `Change` event.
- **Secret hygiene by default** - `secret.String` / `secret.Bytes` redact themselves in `String()`, `fmt`, `MarshalJSON`, and `slog`. Only the explicit, greppable `Reveal()` exposes the value. A shipped `go vet` analyzer (`reconcilevet`) flags sensitive refs assigned to plain `string` fields.
- **Pluggable** - providers register with the `database/sql` pattern; a `providertest` conformance kit guarantees they all behave identically.

## Providers

| Module | Schemes | Watch |
|---|---|---|
| core (built-in) | `env:` · `dotenv://` · `file://` · `exec:` (opt-in) | fsnotify (file/dotenv) · poll (env/exec) |
| `providers/aws` | `aws-sm://` · `aws-ps://` | poll |
| `providers/gcp` | `gcp-sm://` | poll |
| `providers/azure` | `azure-kv://` | poll |
| `providers/vault` | `vault://` | lease-aware poll (`NotAfter`) |
| `providers/k8s` | `k8s-secret://` · `k8s-cm://` | **native** (watch API) |
| `providers/consul` | `consul://` | **native** (blocking queries) |
| `providers/doppler` | `doppler://` | poll |
| `providers/onepassword` | `op://` | poll |
| `providers/sops` | `sops://` | fsnotify |
| `providers/postgres` | `postgres://` | **native** (LISTEN/NOTIFY) |
| `providers/mysql` | `mysql://` | poll |
| `providers/sqlite` | `sqlite://` | fsnotify |
| `providers/mongodb` | `mongodb://` | **native** (change streams) |
| `providers/dynamodb` | `dynamodb://` | poll |
| `providers/redis` | `redis://` | **native** (keyspace notifications) |
| `providers/etcd` | `etcd://` | **native** (watch API) |
| `providers/firestore` | `firestore://` | **native** (snapshot listeners) |
| `providers/firebase-rc` | `firebase-rc://` | poll |
| `providers/firebase-rtdb` | `firebase-rtdb://` | **native** (streaming) |
| `providers/s3` | `s3://` | poll (ETag) |
| `providers/gcs` | `gcs://` | poll (generation) |
| `providers/azblob` | `azblob://` | poll (ETag) |
| `providers/cosmos` | `cosmos://` | poll (ETag) |
| `providers/launchdarkly` | `launchdarkly://` | **native** (streaming) |
| `providers/unleash` | `unleash://` | poll |
| `providers/flagsmith` | `flagsmith://` | poll |
| `providers/configcat` | `configcat://` | poll |
| `providers/split` | `split://` | poll |
| `providers/growthbook` | `growthbook://` | poll |
| `providers/flipt` | `flipt://` | poll |
| `providers/goff` | `goff://` (GO Feature Flag) | poll |

Every provider that passes the [`providertest`](providertest/) conformance kit earns a badge. See each module's README for auth and ref grammar.

## Middleware

Providers compose because they share one interface:

```go
mamori.WithProvider(
    middleware.Cache(5*time.Minute,
        middleware.Audit(logger,
            middleware.Failover(primary, replica))))
```

`Cache`, `Audit`, `Failover`, `RateLimit`, and `Prefix` (multi-tenant namespace rewriting) ship in [`middleware/`](middleware/).

## Documentation

- 📖 **Docs site:** https://mamorigo.dev
- 📦 **API reference:** https://pkg.go.dev/github.com/xavidop/mamori
- 🧩 **Write a provider:** [docs/PROVIDER_SPI.md](docs/PROVIDER_SPI.md)
- 🏃 **Runnable example:** [examples/basic](examples/basic)

## Project layout

This is a multi-module monorepo. The core (`github.com/xavidop/mamori`) depends only on `go-playground/validator`, `go-viper/mapstructure`, and `fsnotify`. Each provider is its own module with its own release cadence, so a cloud SDK never leaks into your build unless you use that provider.

## Contributing

Contributions welcome - new providers especially. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [provider author brief](docs/PROVIDER_SPI.md). A provider that passes `providertest` and the conformance kit gets listed here.

## License

[MIT](LICENSE) © Xavier Portilla Edo
