# mamori Azure providers

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](../../providertest)

[mamori](https://github.com/xavidop/mamori) providers for two Azure services:
**Azure Key Vault** secrets and **Azure App Configuration** settings. Import the
module for its side effect to register both schemes:

```go
import _ "github.com/xavidop/mamori/providers/azure" // registers azure-kv:// and azure-appconfig://
```

## Schemes

| Scheme | Backend | Sensitive | Watch |
|---|---|---|---|
| `azure-kv://<vault-name>/<secret-name>[#json-key]?version=<v>` | Key Vault | ✅ | poll |
| `azure-appconfig://<store>/<key>[#json-key][?label=<label>]` | App Configuration | no | poll |

## `azure-kv://`

The `<vault-name>` is expanded to the vault URL
`https://<vault-name>.vault.azure.net`, and `<secret-name>` is fetched with the
[`azsecrets`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets)
SDK.

| Part | Meaning |
| --- | --- |
| `<vault-name>` | Key Vault name (not the full URL) - required |
| `<secret-name>` | Secret name within the vault - required |
| `#json-key` | Optional. Treat the secret value as a JSON object and select this field via `mamori.SelectKey` |
| `?version=<v>` | Optional. Pin a specific secret version. Omit for the latest version |

Resolved values are always marked `Sensitive`. The `Version` is the native Key
Vault secret version (falling back to a content hash if unavailable), so mamori
detects changes cheaply.

```go
type Config struct {
    // Whole secret value (latest version).
    DBPassword string `source:"azure-kv://prod-vault/db-password"`

    // A field from a JSON secret: {"username":"admin","password":"..."}.
    APIPassword string `source:"azure-kv://prod-vault/api-conn#password"`

    // Pin a specific version.
    SigningKey string `source:"azure-kv://prod-vault/signing-key?version=abc123"`
}
```

## `azure-appconfig://`

```text
azure-appconfig://<store>/<key>[#json-key][?label=<label>]
```

The `<store>` name is expanded to the endpoint `https://<store>.azconfig.io`,
and `<key>` (which may itself contain slashes) is fetched with the
[`azappconfig`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig)
SDK.

| Part | Required | What it means |
| --- | --- | --- |
| `<store>` | yes | The App Configuration store name. The endpoint is built as `https://<store>.azconfig.io`. |
| `<key>` | yes | The setting key within that store. May contain `/`. |
| `#json-key` | no | Select one field from a JSON setting value (via `mamori.SelectKey`). |
| `?label=<label>` | no | Select a labelled setting. |

**An absent `?label=` is not a wildcard.** Azure App Configuration treats "no
label" as its own distinct **null label**, not as "any label" or "the latest
label". A setting stored under the null label and a setting stored under the
label `prod` are two different settings that can hold two different values.
This provider passes the label explicitly on every call - the empty string
when `?label=` is absent - so a ref without `?label=` always resolves the
null-labelled setting and never silently falls back onto (or is shadowed by) a
labelled one.

```go
type Config struct {
    // The null-labelled setting.
    LogLevel string `source:"azure-appconfig://my-store/app/log-level"`

    // The setting explicitly labelled "prod".
    LogLevelProd string `source:"azure-appconfig://my-store/app/log-level?label=prod"`

    // A field from a JSON setting value.
    APIPassword string `source:"azure-appconfig://my-store/app/api-conn#password"`
}
```

Resolved values are **never** marked `Sensitive`: App Configuration is a
configuration service, not a secret store. `Value.Version` is the setting's
ETag, falling back to `mamori.VersionHash` if the ETag is unavailable.

### Key Vault references are rejected, not resolved

App Configuration can store a **reference** to a Key Vault secret instead of a
value: a setting with content type
`application/vnd.microsoft.appconfig.keyvaultref+json` whose value is JSON
shaped like `{"uri":"https://<vault>.vault.azure.net/secrets/<name>"}`. This
provider detects that content type and fails the resolve with
`mamori.ErrInvalid`, naming the equivalent `azure-kv://` ref to use instead,
rather than resolving or otherwise following the reference.

This is deliberate, not a missing feature: returning the reference's raw JSON
would hand a caller the literal text `{"uri":"..."}` as, say, their database
password. That value is a non-empty string, so it passes a typical
non-empty-string validation and only fails much later, deep inside whatever
consumes it (a database driver reporting a bogus auth failure, for example),
far from the actual cause. Point a `secret.String` field at an `azure-kv://`
ref for the vault named in the error instead.

## Authentication

Both providers use the **Azure default credential chain**
([`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#NewDefaultAzureCredential)),
which tries, in order: environment variables, workload identity, managed
identity, and the Azure CLI login. No explicit configuration is needed when
running in an environment with an ambient identity (AKS pod identity, an Azure
VM with a managed identity, or a developer machine logged in via `az login`).

- The Key Vault identity needs the `secrets/get` permission (data-plane RBAC
  role **Key Vault Secrets User**, or a matching access policy) on the target
  vault.
- The App Configuration identity needs the **App Configuration Data Reader**
  role (or equivalent) on the target store.

Clients are created lazily, one per vault/store name, on first resolve - so
importing the package and registering both providers performs no I/O and needs
no credentials at init time.

### Explicit configuration

To inject a specific credential, register the provider(s) yourself:

```go
cred, err := azidentity.NewManagedIdentityCredential(nil)
// handle err
cfg, err := mamori.Load[Config](ctx,
    mamori.WithProvider(azure.New(azure.WithCredential(cred))),
    mamori.WithProvider(azure.NewAppConfig(azure.WithAppConfigCredential(cred))),
)
```

Options:

- `azure.WithCredential(cred azcore.TokenCredential)` / `azure.WithClient(c)` -
  Key Vault provider: an explicit credential, or an injected client (a
  pre-built `*azsecrets.Client` or, in tests, an in-memory fake) used for
  every vault.
- `azure.WithAppConfigCredential(cred azcore.TokenCredential)` /
  `azure.WithAppConfigClient(c)` - App Configuration provider: the same two
  knobs, used for every store.

`Option` and `AppConfigOption` are distinct types, one per provider, so they
cannot be mixed up at the call site.

## Watch

Neither Azure Key Vault nor Azure App Configuration's `GetSetting` call offers
a native change notification usable here, so neither provider implements
`WatchableProvider`. mamori polls both instead.

## Error classification

Both providers share one classifier, `classifyAzure`, since App Configuration
returns the same HTTP statuses as Key Vault:

| HTTP status | mamori kind |
|---|---|
| 404 | `not_found` |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

A transport failure (no HTTP response at all) stays `unknown`, since it could
be a client problem rather than a backend one. `*azcore.ResponseError` stays
reachable with `errors.As`.

## Verified vs. needs a live backend

- **Verified in unit tests (no Azure account):** scheme, resolution, JSON
  `#key` selection, not-found → `mamori.ErrNotFound` mapping (Azure 404),
  version handling, context cancellation, concurrency, goroutine hygiene, and
  the full `providertest.Run` conformance suite for both schemes - all run
  against in-memory fakes. For Key Vault: `?version=` pinning and
  `Sensitive == true`. For App Configuration: the null-label-is-not-a-wildcard
  behavior of an absent `?label=`, `Sensitive == false`, and the Key Vault
  reference rejection.
- **Needs a live backend:** end-to-end auth via the default credential chain
  and real vault/store access. A live test is provided behind a build tag and
  is not run in CI:

  ```sh
  MAMORI_AZURE_VAULT=<vault-name> \
  MAMORI_AZURE_SECRET=<secret-name> \
  go test -tags integration -run TestLive ./...
  ```

## Conformance

This module passes the mamori provider conformance kit
([`providertest`](../../providertest)). Run it locally with the workspace
disabled:

```sh
cd providers/azure
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
