---
layout: ../../../layouts/DocsLayout.astro
title: Vault provider
---

# Vault

HashiCorp Vault KV v2, with lease-aware refresh, built on `hashicorp/vault/api`.

| | |
| --- | --- |
| Scheme | `vault://` |
| Module | `github.com/xavidop/mamori/providers/vault` |
| Sensitive | yes |
| Watch | lease-aware poll |
| Auth | `VAULT_ADDR`, `VAULT_TOKEN` |

## Install

```bash
go get github.com/xavidop/mamori/providers/vault
```

```go
import _ "github.com/xavidop/mamori/providers/vault"
```

## Using the ref

A `vault://` ref points at one secret in a KV v2 engine (or a dynamic/leased secret path).

```text
vault://<mount>/<path>[#key][?renew=true]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<mount>` | yes | The KV v2 mount point, e.g. `secret`. |
| `<path>` | yes | The secret path within that mount. mamori reads the physical `<mount>/data/<path>` for you; a leading `data/` is tolerated, so `vault://secret/app` and `vault://secret/data/app` are equivalent. |
| `#key` | no | Return one field of the secret's data map. Without it, the whole data map is returned as JSON. |
| `?renew=true` | no | Renew a renewable lease on read (dynamic secrets), so the refresh deadline follows the renewed lease. |

**Examples**

- `vault://secret/app#password` selects the `password` field of the KV v2 secret at `secret/app`.
- `vault://secret/app` returns the whole `secret/app` data map as JSON - decode it with `flatten:"json"`.
- `vault://database/creds/readonly#password?renew=true` reads a dynamic database credential and keeps its lease renewed.

```go
type Config struct {
	// KV v2 at mount "secret", path "app"; select the "password" field
	DBPassword secret.String `source:"vault://secret/app#password"`
	// dynamic/leased secret, renewed automatically
	Lease      secret.String `source:"vault://database/creds/readonly#password?renew=true"`
}
```

Values are always `Sensitive`, and `Value.Version` is the KV v2 metadata version (a content hash for reads without version metadata).

## Leases and refresh

When the read secret carries a lease (`LeaseDuration > 0`), the provider sets `Value.NotAfter = now + LeaseDuration`, and mamori schedules a refresh **before** the lease expires rather than waiting for the poll interval. `?renew=true` renews a renewable lease via `Sys().Renew` and derives `NotAfter` from the renewed lease.

Vault KV has no native push, so the provider does not implement `Watch`; mamori polls, and `NotAfter` drives lease-aware refresh.

## Explicit configuration

```go
import vaultprov "github.com/xavidop/mamori/providers/vault"

mamori.WithProvider(vaultprov.New(
	vaultprov.WithAddress("https://vault.internal:8200"),
	vaultprov.WithToken(os.Getenv("VAULT_TOKEN")),
	vaultprov.WithNamespace("team-a"),
))
```

Verified by unit tests (with/without `#key`, lease `NotAfter`) and the conformance kit against an in-memory fake; a dev-Vault integration test is provided behind `//go:build integration`.
