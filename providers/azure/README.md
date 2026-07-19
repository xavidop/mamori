# mamori Azure Key Vault provider

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](../../providertest)

A [mamori](https://github.com/xavidop/mamori) provider for **Azure Key Vault**
secrets. Import it for its side effect to register the `azure-kv` scheme:

```go
import _ "github.com/xavidop/mamori/providers/azure"
```

## Scheme

```
azure-kv://<vault-name>/<secret-name>[#json-key]?version=<v>
```

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

## Ref examples

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

## Authentication

Authentication uses the **Azure default credential chain**
([`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#NewDefaultAzureCredential)),
which tries, in order: environment variables, workload identity, managed
identity, and the Azure CLI login. No explicit configuration is needed when
running in an environment with an ambient identity (AKS pod identity, an Azure
VM with a managed identity, or a developer machine logged in via `az login`).

The identity needs the `secrets/get` permission (data-plane RBAC role
**Key Vault Secrets User**, or a matching access policy) on the target vault.

Clients are created lazily, one per vault name, on first resolve - so importing
the package and registering the provider performs no I/O and needs no
credentials at init time.

### Explicit configuration

To inject a specific credential, register the provider yourself:

```go
cred, err := azidentity.NewManagedIdentityCredential(nil)
// handle err
cfg, err := mamori.Load[Config](ctx,
    mamori.WithProvider(azure.New(azure.WithCredential(cred))),
)
```

Options:

- `azure.WithCredential(cred azcore.TokenCredential)` - use an explicit
  credential instead of the default chain.
- `azure.WithClient(c)` - inject a pre-built client (or an in-memory fake in
  tests) used for every vault.

## Watch

Azure Key Vault has **no native change notification** for secrets, so this
provider does not implement `WatchableProvider`. mamori polls it on the
configured interval instead.

## Verified vs. needs a live backend

- **Verified in unit tests (no Azure account):** scheme, resolution, JSON
  `#key` selection, `?version=` pinning, not-found → `mamori.ErrNotFound`
  mapping (Azure 404), sensitivity, version monotonicity, context cancellation,
  concurrency, goroutine hygiene, and the full `providertest.Run` conformance
  suite - all run against an in-memory fake `kvClient`.
- **Needs a live backend:** end-to-end auth via the default credential chain and
  real vault access. A live test is provided behind a build tag and is not run
  in CI:

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
