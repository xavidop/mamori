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

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Redis. It releases the go-redis client, including its connection pool, that this provider built lazily. A client injected with `WithClient` belongs to the caller and is left open; `New` followed by `Close` with no prior `Resolve` never dials, so there is nothing to release.

`Close` does not stop a `Watch` that is already running, and on a client this provider built itself that watch can go quiet rather than fail: no error reaches your handler, and `Watcher.Get()` keeps serving the last value it saw, indefinitely. A client injected with `WithClient` is never closed, so a watch running on one keeps delivering live events. Either way, cancel the watch's own context to stop it; `Close` is not a substitute. [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) compares every provider.

## Error classification

Beyond the not-found case, other `GET` failures are classified using go-redis's typed predicates so `mamori.ErrorKind` can distinguish them:

| Redis condition | mamori kind |
|---|---|
| `NOAUTH`, `WRONGPASS` (missing/bad credentials) | `unauthenticated` |
| `NOPERM` (ACL denies the command) | `permission_denied` |
| `LOADING`, `CLUSTERDOWN`, `MASTERDOWN` | `unavailable` |
| dial failure (connection refused, DNS failure, ...) | `unavailable` |
| anything else | `unknown` |

Redis has no rate-limit error on this path, so nothing maps to `rate_limited`. Unlike Postgres or MySQL, go-redis has no single error-code field to switch on, so detection uses its exported `IsAuthError`/`IsPermissionError`/`IsLoadingError`/`IsClusterDownError`/`IsMasterDownError` predicates, which recognize the underlying error even when wrapped. A raw dial failure isn't a `redis.Error` at all, so it's detected separately as a `*net.OpError` with `Op == "dial"` - the same signal go-redis's own retry logic uses internally.

Verified with an in-memory fake.
