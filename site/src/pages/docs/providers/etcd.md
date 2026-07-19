---
layout: ../../../layouts/DocsLayout.astro
title: etcd provider
---

# etcd

The etcd v3 key-value store, with **native watch**.

| | |
| --- | --- |
| Scheme | `etcd://` |
| Module | `github.com/xavidop/mamori/providers/etcd` |
| Sensitive | no |
| Watch | native |
| Auth | `ETCD_ENDPOINTS` (or `WithEndpoints`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/etcd
```

```go
import _ "github.com/xavidop/mamori/providers/etcd"
```

## Using the ref

An `etcd://` ref points at one key in the etcd v3 store, optionally selecting a field from a JSON value stored there.

```text
etcd://<key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<key>` | yes | The etcd key, e.g. `service/endpoint`. A fully-slashed form (`etcd:///service/endpoint`) keeps the leading slash, addressing keys under a leading-`/` namespace. |
| `#json-key` | no | When the value is a JSON object, return one field from it (via `mamori.SelectKey`). |

**Examples**

- `etcd://service/endpoint` - reads the raw value stored at key `service/endpoint`.
- `etcd://service/db#max_conns` - reads the JSON object at `service/db` and returns its `max_conns` field.
- `etcd:///features/flags#dark_mode` - returns the `dark_mode` field of the JSON at the leading-slash key `/features/flags`.

```go
type Config struct {
	Endpoint string `source:"etcd://service/endpoint"`
	MaxConns int    `source:"etcd://service/db#max_conns"` // key holds JSON
}
```

`Value.Version` is the key's `ModRevision`, etcd's native per-key revision, so change detection is exact and monotonic. etcd holds configuration rather than managed secrets, so values are non-sensitive; wrap a field in `secret.String` if you want redaction anyway.

## Watch

`Watch` uses the etcd v3 `Watch` API, a genuine server push: it emits an `Update` on every PUT to the key and closes cleanly on context cancellation.

## Configuration

```go
import etcdprov "github.com/xavidop/mamori/providers/etcd"

mamori.WithProvider(etcdprov.New(etcdprov.WithEndpoints("etcd-0:2379", "etcd-1:2379")))
```

Verified with an in-memory fake supporting Get and Watch, so the watch conformance checks run for real. A live-etcd integration test is provided behind `//go:build integration`.
