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

## Error classification

Beyond the not-found case (an empty `Kvs` slice, never a gRPC code), other `Get`/watch failures are classified by gRPC status:

| gRPC code | mamori kind |
| --- | --- |
| `PermissionDenied` | `permission_denied` |
| `Unauthenticated` | `unauthenticated` |
| `Unavailable`, `DeadlineExceeded` | `unavailable` |
| `ResourceExhausted` | `rate_limited` |
| `InvalidArgument` | `unknown` (deliberately unmapped) |
| anything else | `unknown` |

`InvalidArgument` is deliberately left unmapped: etcd reports a bad username/password (`rpctypes.ErrGRPCAuthFailed`) as `InvalidArgument`, the same code ordinary malformed requests use, so there is no way to tell the two apart from the code alone. Mapping it either way would be wrong about half the time, so it stays `unknown`. `codes.NotFound` is never returned by etcd for a missing key either; the local empty-`Kvs` check drives `not_found` instead.

The etcd v3 client also rewrites a fixed set of well-known server error messages (permission denied, invalid auth token, no leader/timed out, no space/too many requests) into an `rpctypes.EtcdError` that does not implement the `GRPCStatus()` interface `status.Code` relies on. The classifier falls back to `errors.As`-ing into `rpctypes.EtcdError` when `status.Code` reports `Unknown`, so these still classify correctly against a live server instead of silently reporting `unknown`.

## Watch

`Watch` uses the etcd v3 `Watch` API, a genuine server push: it emits an `Update` on every PUT to the key and closes cleanly on context cancellation.

## Configuration

```go
import etcdprov "github.com/xavidop/mamori/providers/etcd"

mamori.WithProvider(etcdprov.New(etcdprov.WithEndpoints("etcd-0:2379", "etcd-1:2379")))
```

Verified with an in-memory fake supporting Get and Watch, so the watch conformance checks run for real. A live-etcd integration test is provided behind `//go:build integration`.

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting etcd. It releases the etcd client this provider dialed lazily, including its gRPC connection and any watcher/lease goroutines built on top of it. A client injected with `WithClient` belongs to the caller and is left open; `New` followed by `Close` with no prior `Resolve` never dials, so there is nothing to release.

A `Watch` that was **already running** when `Close` was called is a different case and is **not** covered by that guarantee - and etcd's version of it is the dangerous one. Closing a self-dialed client closes the watch channel underneath that running watch; this provider's watch loop treats a closed channel as a plain return with no error emitted, and mamori's reconciler does the same with the resulting closed `Update` channel, so your `OnError` handler never fires and `Watcher.Get()` goes on serving the last value it saw, indefinitely. (etcd may, less commonly, deliver one final error update first, when its internal watch loop happens to exit through its error path instead - the silent case dominates but neither is a guarantee.) A client injected with `WithClient` is never closed, so a watch running on one keeps delivering live events. Either way, cancelling that watch's own context is the only reliable way to shut it down; never reach for `Close` to stop a `Watch`. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for what every other provider does here.
