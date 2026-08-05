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

## Error classification

Beyond the not-found case above, this provider has no SDK-specific error taxonomy to classify against, so any other backend failure reports `unknown` via `mamori.ErrorKind`. `Resolve` still wraps the backend error with `%w`, so the classification chain (`errors.Is`, `errors.As`) is preserved rather than flattened, even though nothing here maps it to a more specific kind.

`Close()` marks the provider closed; afterwards every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting the Realtime Database. It releases no connection: this provider holds no `*http.Client` of its own, and a running `Watch`'s SSE stream is owned end to end by that watch's own goroutine, which already closes the stream on context cancellation. `Close` deliberately adds no second teardown path for a running watch.

`Close` does not stop a `Watch` that is already running. Its stream stays open, but from then on every change the server pushes arrives as an error update carrying `errors.Is(err, mamori.ErrUnavailable)` instead of a value. Cancel the watch's own context to stop it. [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) compares every provider.

## Watch

`Watch` uses the Realtime Database streaming endpoint (server-sent events): the server pushes `put` and `patch` events as the data changes, and mamori emits an update on each. This is a genuine realtime subscription.

One streamed change is capped at 1 MiB by default. The database opens every stream with a `put` of the whole watched node, so that cap is in practice the largest node you can watch: a bigger node reports `Update{Err: ...}` on every attempt instead of ever delivering a change. Raise it with `WithMaxFrameBytes(n)` to watch a node that is legitimately larger. `Resolve` is unaffected either way.

A dropped connection is reconnected after a wait that starts at `WithReconnectBackoff` (default 2s), doubles on each attempt that delivers nothing, caps at 30s, and drops back to the floor as soon as a stream delivers something.

## Configuration

```go
import rtdbprov "github.com/xavidop/mamori/providers/firebase-rtdb"

mamori.WithProvider(rtdbprov.New(
	rtdbprov.WithDatabaseURL("https://my-project-default-rtdb.firebaseio.com"),
	rtdbprov.WithReconnectBackoff(2*time.Second), // optional; base reconnect wait
	rtdbprov.WithMaxFrameBytes(4<<20),            // optional; ceiling on one streamed change
))
```

Verified with an in-memory fake stream; live behavior is covered by `//go:build integration` tests.
