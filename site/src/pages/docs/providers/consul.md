---
layout: ../../../layouts/DocsLayout.astro
title: Consul provider
---

# Consul

Consul KV, with **native watch** via blocking queries, built on `hashicorp/consul/api`.

| | |
| --- | --- |
| Scheme | `consul://` |
| Module | `github.com/xavidop/mamori/providers/consul` |
| Sensitive | no |
| Watch | native (blocking queries) |
| Auth | `CONSUL_HTTP_ADDR`, `CONSUL_HTTP_TOKEN` |

## Install

```bash
go get github.com/xavidop/mamori/providers/consul
```

```go
import _ "github.com/xavidop/mamori/providers/consul"
```

## Using the ref

A `consul://` ref points at one key in the Consul KV store, optionally selecting a field from a JSON value stored there.

```text
consul://<kv-path>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<kv-path>` | yes | The Consul KV key path, e.g. `config/service/endpoint`. No leading slash. |
| `#json-key` | no | When the stored value is a JSON object, return one field from it (via `mamori.SelectKey`). |

**Examples**

- `consul://config/service/endpoint` - reads the raw value stored at key `config/service/endpoint`.
- `consul://config/service/db#max_conns` - reads the JSON object at `config/service/db` and returns its `max_conns` field.
- `consul://features/flags#dark_mode` - returns the `dark_mode` field of the JSON at `features/flags`.

```go
type Config struct {
	Endpoint string `source:"consul://config/service/endpoint"`
	// a JSON value stored at one key, then a field selected from it
	MaxConns int    `source:"consul://config/service/db#max_conns"`
}
```

`Value.Version` is the KV pair's `ModifyIndex`, Consul's native revision, so change detection is exact and cheap. Consul KV holds configuration rather than managed secrets, so values are non-sensitive; wrap a field in `secret.String` if you want redaction anyway.

## Watch

`Watch` uses Consul **blocking queries**: it re-issues `KV.Get` with `WaitIndex` set to the last `ModifyIndex`, so the request parks on the server until the value changes (or a wait timeout elapses), then emits an `Update`. It handles index resets, backs off on transient errors, and closes on context cancellation.

## Explicit configuration

```go
import consulprov "github.com/xavidop/mamori/providers/consul"

mamori.WithProvider(consulprov.New(
	consulprov.WithAddress("consul.internal:8500"),
	consulprov.WithToken(os.Getenv("CONSUL_HTTP_TOKEN")),
))
```

Verified by unit tests and the conformance kit against an in-memory fake that reproduces blocking-query semantics, so the watch checks run for real. A live-Consul integration test is provided behind `//go:build integration`.
