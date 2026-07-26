---
layout: ../../layouts/DocsLayout.astro
title: Why mamori
---

# Why mamori

The primitives already exist. `gocloud.dev/runtimevar` watches a single variable. Viper and koanf do multi-source config. The AWS caching client and Vault's `LifetimeWatcher` each refresh one backend. Nobody composed them into typed, validated, watchable config with a provider ecosystem, so every production service ends up hand-rolling a `ConfigManager` with a ticker, a mutex, and a prayer.

mamori is that glue, done once. It is the External Secrets Operator provider model one layer down: a library **inside your process**, not an operator inside your cluster.

## Compared to the alternatives

| | Typed struct + tags | Multi-source | Secrets first-class | Runtime watch | Diff-aware callback | Provider ecosystem |
| --- | --- | --- | --- | --- | --- | --- |
| **mamori** | yes | yes | yes | native + poll | yes | yes, with a conformance kit |
| `runtimevar` | no | one var at a time | weak | yes | no | driver matrix |
| Viper / koanf | yes | yes | bolted on | afterthought | no | config-first |
| AWS SM cache / Vault `LifetimeWatcher` | no | single backend | native | native, per backend | no | siloed |
| envconfig / caarlos0/env | yes | env only | no | load-once | no | no |

The operational layer is a second axis the alternatives mostly leave to you: knowing whether config is healthy, expressing precedence between sources, and classifying why a resolve failed.

| | Precedence chains | Health introspection | Error classification | Pre-deploy check | Fan-out server |
| --- | --- | --- | --- | --- | --- |
| **mamori** | per-field, ordered, with `onfail` | `Status` / `Health` / HTTP admin endpoint | `errors.Is` to a typed kind, conformance-enforced | `Doctor` (library) and `mamori doctor` (live) | optional `mamori://` config server |
| `runtimevar` | no | per-variable value only | driver-specific | no | no |
| Viper / koanf | key override order, no per-key policy | no | no | no | no |
| AWS SM cache / Vault `LifetimeWatcher` | no | per backend | backend-specific | no | no |
| envconfig / caarlos0/env | no | no | no | no | no |

- **Precedence chains**: a `source` tag can list several refs (`env:X,aws-sm://x`); the first that resolves wins, and `onfail` (keeplast / useDefault / fail) governs what happens when the winner later errors. Viper and koanf resolve a merged key space rather than an ordered per-field chain, and neither carries a failure policy.
- **Health introspection and error classification**: every resolve error maps to a typed `Kind` (`permission_denied`, `unavailable`, ...) that survives `errors.Is`, and `Status`/`Health` expose it live. `mamori.Doctor` runs the same wiring in CI before a deploy; `mamori doctor` queries a running process's admin endpoint. The alternatives surface a raw backend error, if any, and leave "is my config healthy" to you.

## Where mamori fits

- **gocloud.dev/runtimevar** is the closest primitive. mamori adds struct composition, tags, validation, diff callbacks, and secret hygiene. A `runtimevar` bridge provider could even inherit its driver matrix.
- **External Secrets Operator** solves the same provider problem at the cluster layer by materializing Kubernetes Secrets. mamori is complementary: it is for apps that want to skip the Kubernetes Secret hop, or that do not run on Kubernetes at all. It keeps no persistent external state, so there is no finalizer lifecycle to manage.
- **Viper / koanf** are config-first with secrets bolted on. mamori is secrets-first with config included.
- **spring-cloud-config** and **.NET `IOptionsMonitor<T>`** are the developer-experience benchmark from other ecosystems. `Watch().Get()` is mamori's `IOptionsMonitor<T>.CurrentValue`.

## What mamori is not

- Not a secrets store: no encryption at rest, and no persistent state of its own. The optional [config server](/docs/server/) is a read-through fan-out in front of your existing backends, not a place secrets live, so it does not make mamori a store.
- Not a sync engine between stores (that is ESO / vals / teller territory).
- Not a general feature-flag system, though a flags provider could be built on top.
- Not cross-language: it is deliberately Go-idiomatic.
