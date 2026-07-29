---
layout: ../../../layouts/DocsLayout.astro
title: Azure AppConfig provider
---

# Azure AppConfig

Azure App Configuration, built on the `azappconfig` SDK. Ships in the same module as [Azure Key Vault](/docs/providers/azure).

| | |
| --- | --- |
| Scheme | `azure-appconfig://` |
| Module | `github.com/xavidop/mamori/providers/azure` |
| Sensitive | no |
| Watch | poll |
| Auth | `DefaultAzureCredential` |

## Install

```bash
go get github.com/xavidop/mamori/providers/azure
```

```go
import _ "github.com/xavidop/mamori/providers/azure" // registers azure-kv:// and azure-appconfig://
```

## Using the ref

An `azure-appconfig://` ref points at one setting in an Azure App Configuration store, under a specific (or the null) label.

```text
azure-appconfig://<store>/<key>[#json-key][?label=<label>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<store>` | yes | The App Configuration store name. The endpoint is built as `https://<store>.azconfig.io`. |
| `<key>` | yes | The setting key within that store. May itself contain `/`. |
| `#json-key` | no | Select one field from a JSON setting value (via `mamori.SelectKey`). |
| `?label=<label>` | no | Select a labelled setting. Absent means the null label - see below. |

**Examples**

- `azure-appconfig://my-store/app/log-level` reads the null-labelled `app/log-level` setting.
- `azure-appconfig://my-store/app/log-level?label=prod` reads the same key under the `prod` label - a different setting.
- `azure-appconfig://my-store/app/conn#password` selects the `password` field of a JSON setting value.

```go
type Config struct {
	LogLevel     string `source:"azure-appconfig://my-store/app/log-level"`             // null label
	LogLevelProd string `source:"azure-appconfig://my-store/app/log-level?label=prod"`   // "prod" label
	APIPassword  string `source:"azure-appconfig://my-store/app/conn#password"`          // JSON field
}
```

Values are never marked `Sensitive`: App Configuration is a configuration service, not a secret store. `Value.Version` is the setting's ETag, falling back to `mamori.VersionHash` if unavailable.

### An absent `?label=` is the null label, not a wildcard

Azure App Configuration treats "no label" as its own distinct **null label**, a setting in its own right, not as "any label" or "whichever label was written most recently." A setting stored under the null label and a setting stored under the label `prod` are two different settings that can hold two different values at the same time.

This provider always passes the label explicitly to the SDK - the empty string when `?label=` is absent from the ref - rather than leaving the option unset. A ref without `?label=` therefore always resolves the null-labelled setting specifically, and can never be silently satisfied by a same-keyed setting under a different label.

### Key Vault references are rejected, not resolved

App Configuration can store a setting whose value is a **reference** to a Key Vault secret rather than the value itself: content type `application/vnd.microsoft.appconfig.keyvaultref+json`, value shaped like `{"uri":"https://<vault>.vault.azure.net/secrets/<name>"}`. This provider detects that content type and fails the resolve with `mamori.ErrInvalid`, naming the equivalent [`azure-kv://`](/docs/providers/azure) ref in the error message, instead of resolving or following the reference.

This is a deliberate refusal, not a missing feature. Returning the reference's raw JSON would hand a caller the literal text `{"uri":"..."}` as, say, a database password - a non-empty string that passes typical validation and only fails much later, deep inside whatever consumes it, far from the actual cause. Point a `secret.String` field at the named `azure-kv://` ref instead.

## Explicit configuration

Authentication uses `DefaultAzureCredential` (environment, managed identity, Azure CLI, ...). Inject a credential or client explicitly for tests or non-default auth:

```go
import azureprov "github.com/xavidop/mamori/providers/azure"

mamori.WithProvider(azureprov.NewAppConfig(azureprov.WithAppConfigCredential(myCred)))
```

`AppConfigOption` is a distinct type from Key Vault's `Option`, so the two providers' configuration cannot be mixed up at the call site.

## Watch

App Configuration's `GetSetting` call has no native change-notification surface usable here, so this provider does not implement `WatchableProvider`. mamori polls it (`WithPollInterval` + jitter, `Value.Version` comparison).

## Error classification

Failures are classified with `classifyAzure`, the same function used by [`azure-kv://`](/docs/providers/azure) - App Configuration returns the same HTTP statuses as Key Vault:

| HTTP status | mamori kind |
|---|---|
| 404 | `not_found` |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

A transport failure (no HTTP response at all) stays `unknown`, since it could be a client problem rather than a backend one. `*azcore.ResponseError` stays reachable with `errors.As`.

Verified by unit tests and the conformance kit against an in-memory fake, including the null-label behavior and the Key Vault reference rejection above.
