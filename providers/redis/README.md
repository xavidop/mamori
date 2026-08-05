# mamori - Redis provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from the [Redis](https://redis.io/) key/value store, with **native
hot-reload** driven by Redis keyspace notifications. Built on `go-redis/v9`.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/redis"
```

Importing the package registers the `redis` scheme with mamori. The client is
built lazily on first use from `REDIS_URL` (or the configured address), so
importing the package never contacts Redis.

## Scheme

```
redis://<key>[#json-key]
```

- `<key>` - the Redis key to `GET`, e.g. `config/app/log_level`. Redis keys have
  no directory structure of their own; the `/`-separated form is just a naming
  convention.
- `#json-key` - optional. When present, the stored value is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same
  behavior as every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `redis://config/app/log_level` | Raw value of key `config/app/log_level` |
| `redis://config/app/db#password` | Field `password` of the JSON object at `config/app/db` |

```go
type Config struct {
    LogLevel   string `source:"redis://config/app/log_level"`
    DBPassword string `source:"redis://config/app/db#password"`
}
```

### Value semantics

- `Value.Bytes` - the raw string value returned by `GET`.
- `Value.Version` - Redis has no per-key revision counter, so the version is a
  hash of the value bytes (`mamori.VersionHash`), giving mamori cheap change
  detection without a byte-by-byte comparison.
- `Value.Sensitive` - always `false`. Redis is typically used for configuration
  and caches rather than managed secrets, and this provider has no
  `WithSensitive` option. Wrap a field in `secret.String` if you want redaction
  anyway.
- A missing key returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.

## Error classification

Beyond the not-found case above, other `GET` failures are classified so
`mamori.ErrorKind` can distinguish them. go-redis v9 has no single error-code
field to switch on the way Postgres exposes SQLSTATE; instead it exports typed
predicates that each recognize one wire-protocol error, even when the error
arrives wrapped:

| Redis condition | Detected via | mamori kind |
| --- | --- | --- |
| `NOAUTH`, `WRONGPASS` (missing/bad credentials) | `redis.IsAuthError` | `unauthenticated` |
| `NOPERM` (ACL denies the command) | `redis.IsPermissionError` | `permission_denied` |
| `LOADING` (server still loading its dataset) | `redis.IsLoadingError` | `unavailable` |
| `CLUSTERDOWN` | `redis.IsClusterDownError` | `unavailable` |
| `MASTERDOWN` | `redis.IsMasterDownError` | `unavailable` |
| dial failure (connection refused, DNS failure, ...) | `*net.OpError` with `Op == "dial"` | `unavailable` |
| anything else | - | `unknown` |

Redis has no rate-limit error on this path, so nothing maps to `rate_limited`.
Errors not listed above report `unknown` rather than being guessed at. The dial
check is not one of go-redis's `Is*Error` predicates - a dial failure never
satisfies any of them, because it isn't a `redis.Error` at all - but it uses the
same `errors.As(&opErr) && opErr.Op == "dial"` test go-redis's own retry logic
(`shouldRetry` in `error.go`) already relies on to recognize "the TCP connection
was never established," so it is trusted here as a stable, idiomatic signal
rather than a guess.

## Authentication & configuration

The provider builds its client lazily, in order of precedence:

1. `WithURL(url)` - a full `redis://[:password@]host:port/db` connection URL.
2. The `REDIS_URL` environment variable, parsed the same way.
3. `WithAddr(addr)` (default `127.0.0.1:6379`), with no auth.

| Variable | Purpose |
| --- | --- |
| `REDIS_URL` | Connection URL, e.g. `redis://:mypassword@redis:6379/0` |

For explicit configuration, construct the provider yourself and register it:

```go
p := redis.New(redis.WithAddr("redis:6379"), redis.WithDB(0))
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom client (TLS, auth, pooling, or a `*redis.ClusterClient`
/ `*redis.Ring` instead of a single `*redis.Client`):

```go
client := goredis.NewClient(&goredis.Options{
    Addr:     "redis:6379",
    Password: os.Getenv("REDIS_PASSWORD"),
})
p := redis.New(redis.WithClient(client))
```

### Options

| Option | Effect |
| --- | --- |
| `WithURL(url)` | Connection URL (overrides `REDIS_URL` and `WithAddr`) |
| `WithAddr(addr)` | Host:port to connect to (default `127.0.0.1:6379`); ignored if a URL or client is supplied |
| `WithDB(db)` | Logical database number; also selects the `__keyspace@<db>__` channel `Watch` subscribes to |
| `WithClient(goredis.UniversalClient)` | Inject a pre-configured client (`*redis.Client`, `*redis.ClusterClient`, or `*redis.Ring`) |

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and
any `Watch` started after `Close`, report
`errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Redis. It
releases the go-redis client, including its connection pool, that this
provider built lazily. A client injected with `WithClient` belongs to the
caller and is left open; `New` followed by `Close` with no prior `Resolve`
never dials, so there is nothing to release.

A `Watch` that was **already running** when `Close` was called is a different
case and is **not** covered by that guarantee - and Redis's version of it is
the dangerous one. Closing a self-built client ends the subscription channel
underneath that running watch; this provider's watch loop treats a closed
channel as a plain return with no error emitted, and mamori's reconciler does the same with
the resulting closed `Update` channel, so your `OnError` handler never fires
and `Watcher.Get()` goes on serving the last value it saw, indefinitely. A
client injected with `WithClient` is never closed, so a watch running on one
keeps delivering live events. Either way, cancelling that watch's own context
is the only reliable way to shut it down; never reach for `Close` to stop a
`Watch`. See
[Close does not stop a Watch](https://mamorigo.dev/docs/writing-a-provider/#close-does-not-stop-a-watch)
for what every other provider does here.

## Native watch (keyspace notifications)

The provider implements `mamori.WatchableProvider` using **Redis keyspace
notifications** - the idiomatic Redis push mechanism, not a polling ticker:

1. On `Watch`, the provider `PSUBSCRIBE`s to `__keyspace@<db>__:<key>` *before*
   emitting the baseline, so no notification is missed between the baseline
   `GET` and the start of the read loop.
2. The current value (or its not-found error) is emitted immediately as a
   baseline `Update`.
3. On every notification for the key (`set`, `del`, `expired`, ...) the provider
   re-runs `GET` and emits a fresh `Update` with the authoritative value.
4. When the watch context is cancelled the subscription is closed, the
   goroutine exits, and the `Update` channel is closed - no goroutine leaks
   (verified with `goleak`).

> **Keyspace notifications are OFF by default.** The server must be configured
> with, for example:
>
> ```text
> CONFIG SET notify-keyspace-events KEA
> ```
>
> or the equivalent `notify-keyspace-events` entry in `redis.conf`. Without it
> the server never publishes notifications and `Watch` only ever delivers the
> baseline value (`Resolve` and polling still work). `KEA` enables keyspace (`K`)
> events for all (`A`) event classes; a narrower mask such as `K$g` (keyspace,
> string and generic commands) is also sufficient.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake `redisAPI` (`go test ./...`) |
| Resolve, JSON `#key` selection, not-found, error classification, context cancellation | **Verified** (unit tests) |
| Error classification (`classifyRedis`, all predicates plus dial-failure detection) | **Verified** (unit tests, including a `Resolve`-level test with a real go-redis-shaped error) |
| Native watch (keyspace-notification baseline + change delivery + cancel/close, no goroutine leak) | **Verified** against the fake (including `-race`) |
| End-to-end against a real Redis server | **Needs a live backend** - this provider does not currently ship a build-tag-gated integration test |

The unit and conformance tests use an in-memory fake that reproduces Redis's
`GET` and keyspace-notification semantics, so `go test ./...` requires **no**
running Redis.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/redis
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
