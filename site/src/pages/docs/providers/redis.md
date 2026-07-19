---
layout: ../../../layouts/DocsLayout.astro
title: Redis provider
---

# Redis

Redis keys, with **native watch** via keyspace notifications.

| | |
| --- | --- |
| Scheme | `redis://` |
| Module | `github.com/xavidop/mamori/providers/redis` |
| Sensitive | no |
| Watch | native (keyspace notifications) |
| Auth | `REDIS_URL` (or `WithAddr` / `WithClient`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/redis
```

```go
import _ "github.com/xavidop/mamori/providers/redis"
```

## Using the ref

A `redis://` ref points at one Redis key, fetched with a single `GET`.

```text
redis://<key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<key>` | yes | The Redis key to read. Its raw string value becomes the value. |
| `#json-key` | no | Treat the value as a JSON object and return one field of it. |

**Examples**

- `redis://app/flags` reads the key `app/flags` and returns its raw string value.
- `redis://app/settings#timeout_ms` treats the key `app/settings` as JSON and returns its `timeout_ms` field - pair it with an `int` field.
- `redis://config/app/db#password` selects the `password` field from a JSON value stored at `config/app/db`.

```go
type Config struct {
	FeatureFlags string `source:"redis://app/flags"`
	Timeout      int    `source:"redis://app/settings#timeout_ms"` // key holds JSON
}
```

`Value.Version` is a content hash of the stored value - Redis has no per-key revision counter - so mamori still gets cheap change detection. Redis usually holds configuration and caches rather than managed secrets, so values are not marked sensitive; wrap a field in `secret.String` if you want redaction anyway.

## Watch

`Watch` subscribes to Redis keyspace notifications and re-reads the key on change. This requires the server to have notifications enabled:

```text
redis-cli config set notify-keyspace-events KEA
```

Set the database index with `WithDB(n)` if your key lives outside DB 0. Without keyspace notifications, use polling (`WithPollInterval`).

## Configuration

```go
import redisprov "github.com/xavidop/mamori/providers/redis"

mamori.WithProvider(redisprov.New(redisprov.WithAddr("redis:6379"), redisprov.WithDB(0)))
```

Verified with an in-memory fake; live behavior against a real Redis is covered by `//go:build integration` tests.
