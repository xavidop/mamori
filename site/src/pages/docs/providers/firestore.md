---
layout: ../../../layouts/DocsLayout.astro
title: Firestore provider
---

# Firestore

Read a field from a Cloud Firestore document, with **native watch** via snapshot listeners.

| | |
| --- | --- |
| Scheme | `firestore://` |
| Module | `github.com/xavidop/mamori/providers/firestore` |
| Sensitive | no |
| Watch | native (snapshot listeners) |
| Auth | Application Default Credentials (`WithProjectID`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/firestore
```

```go
import _ "github.com/xavidop/mamori/providers/firestore"
```

## Using the ref

A `firestore://` ref points at one document in a collection, read by its ID.

```text
firestore://<collection>/<doc>[#field]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<collection>` | yes | The Firestore collection ID. |
| `<doc>` | yes | The document ID within that collection. |
| `#field` | no | Return one top-level field of the document instead of the whole thing. Without it, the whole document comes back as JSON. |

**Examples**

- `firestore://config/service` reads the `service` document in the `config` collection and returns it as JSON.
- `firestore://config/service#endpoint` returns just the `endpoint` field of that document.
- `firestore://config/app#max_retries` returns the `max_retries` field - pair it with an `int` field.

```go
type Config struct {
	Endpoint string `source:"firestore://config/service#endpoint"`
	Whole    string `source:"firestore://config/service"` // whole document as JSON
}
```

A scalar field is returned unquoted; maps and arrays as their JSON encoding. `Value.Version` is the document's `UpdateTime`, computed over the whole document, so a change to any field is detected even for a `#field` ref. Firestore holds application configuration rather than managed secrets, so values are not marked sensitive; wrap a field in `secret.String` for redaction.

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Firestore. It releases the backing client, but only one this provider built itself. A client injected with `WithClient` belongs to the caller and is left open; closing it would reach outside this provider and break whatever else the caller is using it for.

A `Watch` that was **already running** when `Close` was called is a different case and is **not** covered by that guarantee. `Close` never reaches into a running watch to end it, and the snapshot listener captured its client before the loop started, so it never passes the closed check again; what it does from there is decided by the closed client, not by this provider, and it is not guaranteed to report `mamori.ErrUnavailable`. A watch running on a client injected with `WithClient` is unaffected, since `Close` never touches that client. Cancelling the watch's own context is the only reliable way to shut it down. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for the shapes this takes across providers.

## Watch

`Watch` uses `Doc.Snapshots`, Firestore's real-time listener, emitting an update on every change to the document. No polling.

## Error classification

| gRPC code | mamori kind |
|---|---|
| `NotFound` | `not_found` |
| `PermissionDenied` | `permission_denied` |
| `Unauthenticated` | `unauthenticated` |
| `Unavailable`, `DeadlineExceeded` | `unavailable` |
| `ResourceExhausted` | `rate_limited` |
| `InvalidArgument` | `invalid` |
| anything else | `unknown` |

The Firestore client is gRPC-based, so codes map one to one and no string matching is needed. Codes with no clear mamori meaning (`Internal`, `Unimplemented`, `Aborted`, `FailedPrecondition`) report `unknown` rather than being guessed at. The gRPC status stays reachable through `status.Code` on a classified error for both `Resolve` and `Watch` - a listener failure mid-watch is classified the same way a `Resolve` failure would be, instead of surfacing as `unknown`.

## Configuration

```go
import fsprov "github.com/xavidop/mamori/providers/firestore"

mamori.WithProvider(fsprov.New(fsprov.WithProjectID("my-project")))
```

Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
