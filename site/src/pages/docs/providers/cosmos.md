---
layout: ../../../layouts/DocsLayout.astro
title: Cosmos DB provider
---

# Azure Cosmos DB

Read a value from an Azure Cosmos DB document (SQL / Core API).

| | |
| --- | --- |
| Scheme | `cosmos://` |
| Module | `github.com/xavidop/mamori/providers/cosmos` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | poll (ETag) |
| Auth | `DefaultAzureCredential` + endpoint, or a connection string |

## Install

```bash
go get github.com/xavidop/mamori/providers/cosmos
```

```go
import _ "github.com/xavidop/mamori/providers/cosmos"
```

## Using the ref

A `cosmos://` ref points at one item (document) in a container, read by its id and partition key.

```text
cosmos://<database>/<container>/<id>[#field][?pk=<partition-key>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<database>` | yes | The Cosmos database name. |
| `<container>` | yes | The container within that database. |
| `<id>` | yes | The item's `id`. |
| `#field` | no | Return one field of the document instead of the whole thing (JSON selection). |
| `?pk=` | no | The partition-key value. Defaults to `<id>` (common when the id is the partition key). |

**Examples**

- `cosmos://appdb/config/service-a` reads the document with id `service-a` (partition key `service-a`) and returns it as JSON.
- `cosmos://appdb/config/service-a#endpoint` returns just the `endpoint` field of that document.
- `cosmos://appdb/tenants/acme?pk=us-east` reads id `acme` in the `us-east` logical partition.

`Value.Version` is the document's `_etag`, so change detection is cheap. Wrap a field in `secret.String` (or set `WithSensitive`) if the document holds credentials.

## Watch

Cosmos DB's change feed is pull-based, so mamori polls (`WithPollInterval` + jitter) using the ETag. The change feed can drive an on-demand reload in your app for push.

## Configuration

```go
import cosmosprov "github.com/xavidop/mamori/providers/cosmos"

// AAD credential + account endpoint
mamori.WithProvider(cosmosprov.New(cosmosprov.WithEndpoint("https://myacct.documents.azure.com:443/")))
// or a connection string
mamori.WithProvider(cosmosprov.New(cosmosprov.WithConnectionString(os.Getenv("COSMOS_CONNECTION_STRING"))))
```

Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.

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

Cosmos DB returns **429 for request-unit (RU/s) throttling** - its most common operational failure. It maps to `rate_limited` like any other 429, but if you see `rate_limited` on a `cosmos://` ref, check the container's/database's provisioned RU/s first; it is very unlikely to be a client-side rate limiter.

A transport failure (no HTTP response at all) stays `unknown`, since it could be a bug in this provider rather than a genuine backend outage. `*azcore.ResponseError` stays reachable with `errors.As` through the classified fallback path. The not-found path is a special case: the item reader returns the bare `mamori.ErrNotFound` sentinel for a 404 (an internal signal, not the SDK error itself), so there is no `*azcore.ResponseError` to recover there.

Verified by unit tests and the conformance kit against an in-memory fake; live Azure behavior is covered by `//go:build integration` tests.
