---
layout: ../../../layouts/DocsLayout.astro
title: Azure Blob provider
---

# Azure Blob Storage

Fetch a config or secret blob from Azure Blob Storage.

| | |
| --- | --- |
| Scheme | `azblob://` |
| Module | `github.com/xavidop/mamori/providers/azblob` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | poll (ETag) |
| Auth | `DefaultAzureCredential` (+ account URL) |

## Install

```bash
go get github.com/xavidop/mamori/providers/azblob
```

```go
import _ "github.com/xavidop/mamori/providers/azblob"
```

## Using the ref

An `azblob://` ref points at one blob in a container. The storage account is provider configuration, not part of the ref.

```text
azblob://<container>/<blob>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<container>` | yes | The blob container name. |
| `<blob>` | yes | The blob name. It may contain slashes (a virtual-directory path), e.g. `config/app.json`. |
| `#json-key` | no | Treat the blob as a JSON object and return one field of it. |

**Examples**

- `azblob://config/app.json` fetches the whole blob from the `config` container - decode it with `flatten:"json"`.
- `azblob://config/app.json#database` returns just the `database` field of that JSON object.
- `azblob://secrets/tls/app.pem` fetches a nested blob (the blob name carries slashes).

```go
type Config struct {
	AppConfig AppConfig `source:"azblob://config/app.json" flatten:"json"`
}

mamori.WithProvider(azblobprov.New(
	azblobprov.WithAccountURL("https://myaccount.blob.core.windows.net"),
))
```

The storage account is provider-level configuration (set it with `WithAccountURL` / `WithServiceURL`, or the `AZURE_STORAGE_ACCOUNT` environment variable), so the same ref can resolve against different accounts across environments. The blob name may contain slashes, so `config/app.json` is a single name. `Value.Version` is the blob ETag (or version id when versioning is enabled), so change detection is cheap. Blobs are not marked sensitive by default; pass `WithSensitive(true)` for accounts that hold secrets.

## Watch

mamori polls (`WithPollInterval` + jitter) using the ETag. For push, Azure Event Grid blob events can trigger an on-demand reload.

## Configuration

Authentication uses `DefaultAzureCredential` (environment, managed identity, Azure CLI). Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
