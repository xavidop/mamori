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

## Where mamori fits

- **gocloud.dev/runtimevar** is the closest primitive. mamori adds struct composition, tags, validation, diff callbacks, and secret hygiene. A `runtimevar` bridge provider could even inherit its driver matrix.
- **External Secrets Operator** solves the same provider problem at the cluster layer by materializing Kubernetes Secrets. mamori is complementary: it is for apps that want to skip the Kubernetes Secret hop, or that do not run on Kubernetes at all. It keeps no persistent external state, so there is no finalizer lifecycle to manage.
- **Viper / koanf** are config-first with secrets bolted on. mamori is secrets-first with config included.
- **spring-cloud-config** and **.NET `IOptionsMonitor<T>`** are the developer-experience benchmark from other ecosystems. `Watch().Get()` is mamori's `IOptionsMonitor<T>.CurrentValue`.

## What mamori is not

- Not a secrets store: no encryption at rest, no server component.
- Not a sync engine between stores (that is ESO / vals / teller territory).
- Not a general feature-flag system, though a flags provider could be built on top.
- Not cross-language: it is deliberately Go-idiomatic.
