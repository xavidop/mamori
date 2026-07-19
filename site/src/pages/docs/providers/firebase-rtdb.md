---
layout: ../../../layouts/DocsLayout.astro
title: Firebase Realtime Database provider
---

# Firebase Realtime Database

Read a value at a Realtime Database path, with **native watch** via the streaming API.

| | |
| --- | --- |
| Scheme | `firebase-rtdb://` |
| Module | `github.com/xavidop/mamori/providers/firebase-rtdb` |
| Sensitive | no |
| Watch | native (SSE streaming) |
| Auth | Application Default Credentials (`WithDatabaseURL`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/firebase-rtdb
```

```go
import _ "github.com/xavidop/mamori/providers/firebase-rtdb"
```

## Using the ref

A `firebase-rtdb://` ref points at one location (path) in your Realtime Database.

```text
firebase-rtdb://<path>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<path>` | yes | The database location to read, e.g. `config/service/db`. The value at that path becomes the value, as JSON. |
| `#json-key` | no | Treat the value at the path as a JSON object and return one field of it. |

**Examples**

- `firebase-rtdb://config/flags` returns the value at `config/flags` as JSON.
- `firebase-rtdb://config/service/endpoint` reads the `endpoint` leaf under `config/service` (a string leaf comes back unquoted).
- `firebase-rtdb://config/service/db#password` selects the `password` field from the JSON object stored at `config/service/db`.

```go
type Config struct {
	Endpoint string `source:"firebase-rtdb://config/service/endpoint"`
	Flags    string `source:"firebase-rtdb://config/flags"`
}
```

A JSON string leaf is returned unquoted; other JSON (objects, arrays, numbers, booleans) as its JSON encoding. A null or missing path resolves to not-found, so `default:` / `optional:"true"` applies. `Value.Version` is the database ETag when available (an exact native revision), falling back to a content hash. The Realtime Database holds configuration rather than managed secrets, so values are not marked sensitive; wrap a field in `secret.String` for redaction.

## Watch

`Watch` uses the Realtime Database streaming endpoint (server-sent events): the server pushes `put` and `patch` events as the data changes, and mamori emits an update on each. This is a genuine realtime subscription.

## Configuration

```go
import rtdbprov "github.com/xavidop/mamori/providers/firebase-rtdb"

mamori.WithProvider(rtdbprov.New(
	rtdbprov.WithDatabaseURL("https://my-project-default-rtdb.firebaseio.com"),
))
```

Verified with an in-memory fake stream; live behavior is covered by `//go:build integration` tests.
