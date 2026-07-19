# mamori Azure Blob Storage provider

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](../../providertest)

A [mamori](https://github.com/xavidop/mamori) provider for **Azure Blob
Storage**. Import it for its side effect to register the `azblob` scheme:

```go
import _ "github.com/xavidop/mamori/providers/azblob"
```

## Scheme

```
azblob://<container>/<blob>[#json-key]
```

The blob is downloaded with the
[`azblob`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/storage/azblob)
SDK (`DownloadStream`) and its bytes become the resolved value.

| Part | Meaning |
| --- | --- |
| `<container>` | Blob container name - required |
| `<blob>` | Blob name within the container - required. May contain `/` (virtual directories); only the first `/` of the path splits container from blob |
| `#json-key` | Optional. Treat the blob payload as a JSON object and select this field via `mamori.SelectKey` |

The **storage account is provider-level configuration, not part of the ref** (see
[Authentication](#authentication)), so the same ref resolves against different
accounts across environments.

Resolved values are **not** marked `Sensitive` by default - blobs commonly hold
ordinary config. Pass `azblob.WithSensitive(true)` for containers that hold
secret material. The `Version` is the blob **ETag** (or its `VersionID` when
versioning is enabled), falling back to a content hash, so mamori detects
changes cheaply.

## Ref examples

```go
type Config struct {
    // Whole blob body.
    Settings string `source:"azblob://config/app-settings.json"`

    // A nested blob path (the blob name contains slashes).
    ProdDB string `source:"azblob://data/envs/prod/db-url.txt"`

    // A field from a JSON blob: {"username":"admin","password":"..."}.
    APIPassword string `source:"azblob://config/api-conn.json#password"`
}
```

## Authentication

Authentication uses the **Azure default credential chain**
([`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#NewDefaultAzureCredential)),
which tries, in order: environment variables, workload identity, managed
identity, and the Azure CLI login. No explicit configuration is needed when
running in an environment with an ambient identity (an AKS pod identity, an Azure
VM with a managed identity, or a developer machine logged in via `az login`). The
identity needs data-plane read access to the blob (RBAC role **Storage Blob Data
Reader**, or a matching assignment) on the target account.

The **storage account** must be configured, since it is not part of the ref:

- `AZURE_STORAGE_ACCOUNT` environment variable - an account **name** (e.g.
  `mystorageacct`), expanded to `https://mystorageacct.blob.core.windows.net`.
- `azblob.WithAccountURL(url)` / `azblob.WithServiceURL(url)` - a full service
  URL (`https://<account>.blob.core.windows.net`) or a bare account name.

The real client is built lazily on first resolve, so importing the package and
registering the provider performs no I/O and needs no credentials at init time.

### Explicit configuration

To set the account and/or credential yourself, register the provider explicitly:

```go
cred, err := azidentity.NewManagedIdentityCredential(nil)
// handle err
cfg, err := mamori.Load[Config](ctx,
    mamori.WithProvider(azblob.New(
        azblob.WithAccountURL("https://prodacct.blob.core.windows.net"),
        azblob.WithCredential(cred),
        azblob.WithSensitive(true),
    )),
)
```

Options:

- `azblob.WithAccountURL(url)` - blob service endpoint (full URL or account name).
- `azblob.WithServiceURL(url)` - alias for `WithAccountURL`.
- `azblob.WithSensitive(bool)` - mark resolved values `Sensitive` (default false).
- `azblob.WithCredential(cred azcore.TokenCredential)` - use an explicit
  credential instead of the default chain.
- `azblob.WithClient(c)` - inject a custom downloader (or an in-memory fake in
  tests).

## Watch (polling)

Azure Blob Storage has **no cheap native push** for a single blob, so this
provider does **not** implement `WatchableProvider`. mamori polls it on the
configured interval instead. Polling stays cheap because the `Version` is the
blob **ETag**: an unchanged ETag means no new value to decode.

A future push mode could subscribe to
[Event Grid blob events](https://learn.microsoft.com/azure/storage/blobs/storage-blob-event-overview)
(`Microsoft.Storage.BlobCreated`) to get change notifications without polling;
it is not implemented today.

## Verified vs. needs a live backend

- **Verified in unit tests (no Azure account):** scheme, resolution, nested blob
  paths, JSON `#key` selection, not-found → `mamori.ErrNotFound` mapping (both the
  typed `bloberror` codes and a raw 404), default vs. `WithSensitive`, account URL
  normalization, missing-account configuration error, version change on mutate,
  context cancellation, concurrency, goroutine hygiene, and the full
  `providertest.Run` conformance suite - all run against an in-memory fake
  downloader.
- **Needs a live backend:** end-to-end auth via the default credential chain,
  real `DownloadStream`, and native ETag/VersionID handling. A live test is
  provided behind a build tag and is not run in CI:

  ```sh
  MAMORI_AZBLOB_ACCOUNT=<account-name-or-service-url> \
  MAMORI_AZBLOB_CONTAINER=<container> \
  MAMORI_AZBLOB_BLOB=<blob-name> \
  go test -tags integration -run TestLive ./...
  ```

## Conformance

This module passes the mamori provider conformance kit
([`providertest`](../../providertest)). Run it locally with the workspace
disabled:

```sh
cd providers/azblob
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
