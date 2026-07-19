---
layout: ../../../layouts/DocsLayout.astro
title: Azure provider
---

# Azure

Azure Key Vault, built on the `azsecrets` SDK.

| | |
| --- | --- |
| Scheme | `azure-kv://` |
| Module | `github.com/xavidop/mamori/providers/azure` |
| Sensitive | yes |
| Watch | poll |
| Auth | `DefaultAzureCredential` |

## Install

```bash
go get github.com/xavidop/mamori/providers/azure
```

```go
import _ "github.com/xavidop/mamori/providers/azure"
```

## Using the ref

An `azure-kv://` ref points at one secret in an Azure Key Vault, at a specific (or the latest) version.

```text
azure-kv://<vault-name>/<secret-name>[#json-key][?version=<v>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<vault-name>` | yes | The Key Vault name. The URL is built as `https://<vault-name>.vault.azure.net`. |
| `<secret-name>` | yes | The secret name within that vault. |
| `#json-key` | no | Select one field from a JSON secret payload (via `mamori.SelectKey`). |
| `?version=<v>` | no | Pin a specific secret version id. Empty resolves the latest. |

**Examples**

- `azure-kv://my-vault/db-password` reads the latest version of `db-password`.
- `azure-kv://my-vault/api-key?version=abc123` pins the `abc123` version.
- `azure-kv://my-vault/creds#password` selects the `password` field of a JSON secret.

```go
type Config struct {
	DBPassword secret.String `source:"azure-kv://my-vault/db-password"`
	APIKey     secret.String `source:"azure-kv://my-vault/api-key?version=abc123"`
	Nested     secret.String `source:"azure-kv://my-vault/creds#password"`
}
```

Values are always `Sensitive`, and `Value.Version` is the secret's version id (a content hash if unavailable).

## Explicit configuration

Authentication uses `DefaultAzureCredential` (environment, managed identity, Azure CLI, ...). Inject a credential or client explicitly for tests or non-default auth:

```go
import azureprov "github.com/xavidop/mamori/providers/azure"

mamori.WithProvider(azureprov.New(azureprov.WithCredential(myCred)))
```

## Watch

Key Vault has no native change notification, so mamori polls (`WithPollInterval` + jitter).

Verified by unit tests and the conformance kit against an in-memory fake; live Azure behavior is covered by `//go:build integration` tests.
