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

## Watch

`Watch` uses `Doc.Snapshots`, Firestore's real-time listener, emitting an update on every change to the document. No polling.

## Configuration

```go
import fsprov "github.com/xavidop/mamori/providers/firestore"

mamori.WithProvider(fsprov.New(fsprov.WithProjectID("my-project")))
```

Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
