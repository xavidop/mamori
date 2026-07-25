# mamori Azure Cosmos DB provider

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](../../providertest)

A [mamori](https://github.com/xavidop/mamori) provider for **Azure Cosmos DB**
(SQL / Core API), built on
[`azcosmos`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos).
Resolve config values straight from a Cosmos item with a single point read.

```bash
go get github.com/xavidop/mamori/providers/cosmos
```

```go
import _ "github.com/xavidop/mamori/providers/cosmos" // registers cosmos://
```

## Scheme

```
cosmos://<database>/<container>/<id>[#field][?pk=<partitionKeyValue>]
```

| Part | Meaning | Default |
|---|---|---|
| `<database>` | Cosmos database id | - (required) |
| `<container>` | container id within the database | - (required) |
| `<id>` | item id (point read) | - (required) |
| `#field` | select one field from the document JSON via `mamori.SelectKey`; omit for the whole document | whole document |
| `?pk` | partition key **value** for the read | the item `<id>` |

Each ref resolves with a single `ContainerClient.ReadItem` using the item id and
a partition key. The partition key value **defaults to the item id** - the common
case where a container partitions on `/id`. Set `?pk` when the container
partitions on a different value (a tenant id, a category, and so on). A missing
item (HTTP 404), or a `#field` the document lacks, returns an error satisfying
`errors.Is(err, mamori.ErrNotFound)`, so mamori applies defaults / optional
handling.

### What you get back

- **No `#field`** - the whole item document as JSON (the read response body).
- **`#field`** - just that field. String values are returned unquoted; objects,
  arrays, numbers, and booleans as their JSON encoding (via `mamori.SelectKey`).

### Ref examples

```go
type Config struct {
    // Whole document (partition key defaults to the id "app"):
    Settings  string `source:"cosmos://appdb/settings/app"`

    // One field from a document:
    LogLevel  string `source:"cosmos://appdb/settings/app#logLevel"`

    // Explicit partition key value (container partitioned by tenant):
    Feature   string `source:"cosmos://appdb/flags/checkout?pk=tenant-7"`

    // Secret material - mark the value Sensitive (see below).
    APIKey    secret.String `source:"cosmos://appdb/secrets/prod#apiKey"`
}
```

`#field` selects a **top-level field** of the document. To reach into a JSON blob
stored in a single string field, resolve that field and decode it in your own
type.

### Version / change detection

`Value.Version` is the read response **ETag** (Cosmos returns one on every read),
falling back to the document's own `_etag` system field, and finally to a content
hash (`mamori.VersionHash`). The ETag is a whole-document revision, so the version
is derived from the document regardless of which `#field` is projected. Either
way, mamori detects changes cheaply on the next poll: an unchanged ETag means no
new value to decode.

### Sensitivity

Items are **not** marked `Sensitive` by default (containers commonly hold
ordinary config). When a container holds secret material, construct the provider
with `WithSensitive(true)` so resolved values are redacted downstream:

```go
mamori.WithProvider(cosmos.New(cosmos.WithSensitive(true)))
```

## Authentication

Two authentication modes are supported; the real client is built lazily on first
resolve, so importing the package and registering the provider performs no I/O
and needs no credentials at init time.

### Endpoint + DefaultAzureCredential

Set the account **endpoint** and authenticate with the **Azure default credential
chain**
([`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#NewDefaultAzureCredential)),
which tries, in order: environment variables, workload identity, managed
identity, and the Azure CLI login. No explicit configuration is needed when
running with an ambient identity (an AKS pod identity, an Azure VM with a managed
identity, or a developer machine logged in via `az login`). The identity needs
data-plane read access to the container (a Cosmos DB SQL role such as **Cosmos DB
Built-in Data Reader**) on the target account.

- `COSMOS_ENDPOINT` environment variable - e.g.
  `https://<account>.documents.azure.com:443/`.
- `cosmos.WithEndpoint(url)` - the same, set explicitly.
- `cosmos.WithCredential(cred azcore.TokenCredential)` - use an explicit
  credential instead of the default chain.

```go
cfg, err := mamori.Load[Config](ctx,
    mamori.WithProvider(cosmos.New(
        cosmos.WithEndpoint("https://prodacct.documents.azure.com:443/"),
    )),
)
```

### Connection string

Alternatively, authenticate with an account-key **connection string**
(`AccountEndpoint=...;AccountKey=...`). When set, it takes precedence over
endpoint + credential.

- `COSMOS_CONNECTION_STRING` environment variable.
- `cosmos.WithConnectionString(cs)` - the same, set explicitly.

```go
cfg, err := mamori.Load[Config](ctx,
    mamori.WithProvider(cosmos.New(cosmos.WithConnectionString(cs))),
)
```

Options summary:

- `cosmos.WithEndpoint(url)` - account endpoint (used with the credential chain).
- `cosmos.WithConnectionString(cs)` - connection string (account-key auth).
- `cosmos.WithCredential(cred)` - explicit credential instead of the default chain.
- `cosmos.WithSensitive(bool)` - mark resolved values `Sensitive` (default false).
- `cosmos.WithClient(c)` - inject a custom reader (or an in-memory fake in tests).

## Watch (polling)

Azure Cosmos DB has **no cheap native push** for a single item - its change feed
is **pull-based** (you poll it for batches of changes), not a push channel for one
document. So this provider does **not** implement `WatchableProvider`; mamori
polls it on the configured interval instead. Polling stays cheap because the
`Version` is the read **ETag**: an unchanged ETag means no new value to decode.
Configure with `mamori.WithPollInterval`.

> **Future push mode:** the Cosmos DB
> [change feed](https://learn.microsoft.com/azure/cosmos-db/change-feed) can
> deliver item-level changes, but it is pull-based and typically consumed with a
> change-feed processor and a lease container (extra infrastructure). A future
> release may add an opt-in change-feed-backed `Watch`; today the provider stays
> zero-infrastructure and relies on polling.

## Error classification

| HTTP status | mamori kind |
|---|---|
| 404 | `not_found` |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

**Cosmos DB returns 429 for request-unit (RU/s) throttling** - exhausting the
container or database's provisioned throughput - which is this service's most
common operational failure. It maps to `rate_limited` like any other 429, but
if you see `rate_limited` on a `cosmos://` ref, look at the container's/database's
provisioned RU/s first; it is very unlikely to be a client-side rate limiter.

A transport failure (no HTTP response at all, e.g. a DNS failure or a dropped
connection) stays `unknown`, since it could be a bug in this provider as
easily as a genuine backend outage. `*azcore.ResponseError` stays reachable
with `errors.As` through the classified fallback path. The not-found path is
a special case: `sdkReader.ReadItem` returns the bare `mamori.ErrNotFound`
sentinel for a 404 (an internal signal, not the SDK error itself), so there is
no `*azcore.ResponseError` to recover on that path.

## Verified vs. needs a live backend

- **Verified in unit tests (no Cosmos account):** scheme, whole-document
  resolution, JSON `#field` selection, not-found -> `mamori.ErrNotFound` mapping
  (both a raw 404 `*azcore.ResponseError` and a missing `#field`), partition-key
  defaulting to the id and the `?pk` override, `Version` from the response ETag /
  document `_etag` / content-hash fallbacks, default vs. `WithSensitive`,
  missing-account configuration error, error classification (permission denied,
  unauthenticated, rate limited including the RU/s-throttling shape, unavailable,
  invalid, unmapped stays `unknown`), context cancellation, concurrency,
  goroutine hygiene, and the full `providertest.Run` conformance suite - all run
  against an in-memory fake reader.
- **Needs a live backend:** end-to-end auth (DefaultAzureCredential + endpoint, or
  a connection string), real `ReadItem` point reads, partition-key routing, and
  native ETag handling. A live test is provided behind a build tag and is not run
  in CI:

  ```sh
  # endpoint + default credential chain
  MAMORI_COSMOS_ENDPOINT=https://<account>.documents.azure.com:443/ \
  MAMORI_COSMOS_DATABASE=<database> \
  MAMORI_COSMOS_CONTAINER=<container> \
  MAMORI_COSMOS_ID=<item-id> \
  MAMORI_COSMOS_PK=<partition-key-value> \
  go test -tags integration -run TestLive ./...

  # or a connection string
  MAMORI_COSMOS_CONNECTION_STRING="AccountEndpoint=...;AccountKey=..." \
  MAMORI_COSMOS_DATABASE=<database> \
  MAMORI_COSMOS_CONTAINER=<container> \
  MAMORI_COSMOS_ID=<item-id> \
  go test -tags integration -run TestLive ./...
  ```

## Conformance

This module passes the mamori provider conformance kit
([`providertest`](../../providertest)). Run it locally with the workspace
disabled:

```sh
cd providers/cosmos
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
