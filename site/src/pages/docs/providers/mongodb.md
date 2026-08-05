---
layout: ../../../layouts/DocsLayout.astro
title: MongoDB provider
---

# MongoDB

Read a value from a MongoDB document, with **native watch** via change streams.

| | |
| --- | --- |
| Scheme | `mongodb://` |
| Module | `github.com/xavidop/mamori/providers/mongodb` |
| Sensitive | no |
| Watch | native (change streams) |
| Auth | `MONGODB_URI` (+ `WithDatabase`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/mongodb
```

```go
import _ "github.com/xavidop/mamori/providers/mongodb"
```

## Using the ref

A `mongodb://` ref points at one document in a collection, optionally selecting a single field from it.

```text
mongodb://<collection>/<docid>[#field][?key=<field>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<collection>` | yes | The collection to look in. |
| `<docid>` | yes | Identifies the document. By default matched as `_id == <docid>` (as an ObjectID when it is a valid 24-character hex string, otherwise as a plain value). |
| `#field` | no | Return one field of the matched document (via `mamori.SelectKey`). Without it, the whole document is returned as JSON. |
| `?key=<field>` | no | Match the document by an arbitrary field instead of `_id`, i.e. where `<field> == <docid>`. |

**Examples**

- `mongodb://config/service` - returns the whole document with `_id == "service"` as JSON.
- `mongodb://config/service#endpoint` - returns just the `endpoint` field of that document.
- `mongodb://users/507f1f77bcf86cd799439011#email` - returns the `email` field of the user whose ObjectID `_id` is `507f...`.
- `mongodb://users/ada@example.com#apiKey?key=email` - returns `apiKey` for the user whose `email == "ada@example.com"`.

```go
type Config struct {
	Endpoint string `source:"mongodb://config/service#endpoint"`
	Whole    string `source:"mongodb://config/service"` // entire document as JSON
}
```

Per the mamori grammar the `#field` fragment comes before the `?opts` query (`mongodb://coll/id#field?key=email`). `Value.Version` is the document's `version` field when present, otherwise a content hash over the document JSON. Values are non-sensitive; wrap a field in `secret.String` for redaction. Native watch needs a replica set (or sharded cluster) - change streams do not run against a standalone `mongod`, where mamori falls back to polling.

## Watch

`Watch` opens a change stream on the collection filtered to the target document and emits an update on each change. Change streams require the server to be a replica set (or sharded cluster).

## Error classification

Beyond the not-found case, other `FindOne`/change-stream failures are classified by MongoDB server error code so `mamori.ErrorKind` can distinguish them:

| Code | mamori kind |
|---|---|
| `18` (AuthenticationFailed) | `unauthenticated` |
| `13` (Unauthorized) | `permission_denied` |
| anything else | `unknown` |

This table is deliberately small: only auth and authorization are classified today. MongoDB has dozens of replica-set and write codes (`NotWritablePrimary`, `PrimarySteppedDown`, and similar) whose mapping to `unavailable` is arguable and whose reachability on a read is uncertain, so they are left unmapped rather than guessed at. The original `mongo.CommandError` stays reachable with `errors.As`.

## Configuration

```go
import mongoprov "github.com/xavidop/mamori/providers/mongodb"

mamori.WithProvider(mongoprov.New(
	mongoprov.WithURI(os.Getenv("MONGODB_URI")),
	mongoprov.WithDatabase("app"),
))
```

Verified with an in-memory fake; live behavior against a replica set is covered by `//go:build integration` tests.

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting MongoDB. It disconnects the `*mongo.Client` this provider dialed lazily, bounded by a short internal timeout so a wedged server cannot make `Close` hang. A client injected with `WithClient` belongs to the caller and is left connected; `New` followed by `Close` with no prior `Resolve` never dials, so there is nothing to release.

A `Watch` that was **already running** when `Close` was called is a different case and is **not** covered by that guarantee. `Close` never reaches into a running watch to end it, and it captured its client before the loop started, so it never passes the closed check again; what it does from there is decided by the disconnected driver, not by this provider, and it is not guaranteed to report `mamori.ErrUnavailable`. A watch running on a client injected with `WithClient` is unaffected, since `Close` never touches that client. Cancelling the watch's own context is the only reliable way to shut it down. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for the shapes this takes across providers.
