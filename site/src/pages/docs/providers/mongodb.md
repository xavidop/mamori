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

## Configuration

```go
import mongoprov "github.com/xavidop/mamori/providers/mongodb"

mamori.WithProvider(mongoprov.New(
	mongoprov.WithURI(os.Getenv("MONGODB_URI")),
	mongoprov.WithDatabase("app"),
))
```

Verified with an in-memory fake; live behavior against a replica set is covered by `//go:build integration` tests.
