# mamori - etcd provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from the [etcd](https://etcd.io/) v3 key-value store, with **native
hot-reload** driven by etcd's watch stream.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/etcd"
```

Importing the package registers the `etcd` scheme with mamori. The etcd client is
built lazily on first use from the ambient environment, so importing the package
never contacts etcd.

## Scheme

```
etcd://<key>[#json-key]
```

- `<key>` - the etcd key, e.g. `config/app/db`. A fully-slashed form such as
  `etcd:///config/app/db` keeps its leading slash, so you can address keys under
  a leading-`/` namespace.
- `#json-key` - optional. When present, the stored value is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same
  behavior as every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `etcd://config/app/log_level` | Raw value of key `config/app/log_level` |
| `etcd://config/app/db#password` | Field `password` of the JSON object at `config/app/db` |
| `etcd:///features/flags#dark_mode` | Field `dark_mode` of the JSON object at `/features/flags` |

```go
type Config struct {
    LogLevel   string `source:"etcd://config/app/log_level"`
    DBHost     string `source:"etcd://config/app/db#host"`
    DBPassword string `source:"etcd://config/app/db#password"`
    DarkMode   bool   `source:"etcd://features/flags#dark_mode"`
}
```

### Value semantics

- `Value.Bytes` - the raw value bytes stored at the key.
- `Value.Version` - the key's `ModRevision` (decimal string). This is etcd's
  native per-key revision, so change detection is exact and cheap - no byte
  comparison.
- `Value.Sensitive` - always `false`. etcd stores configuration, not managed
  secrets. Wrap a field in `secret.String` if you want redaction anyway.
- A missing key returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

The provider needs one or more etcd endpoints. They are read, in order of
precedence, from:

1. The `WithEndpoints(...)` option.
2. The `ETCD_ENDPOINTS` environment variable (comma-separated).

| Variable | Purpose |
| --- | --- |
| `ETCD_ENDPOINTS` | Comma-separated endpoint list, e.g. `127.0.0.1:2379` or `etcd-a:2379,etcd-b:2379` |

If no endpoints are configured, `Resolve`/`Watch` return an error.

For explicit configuration, construct the provider yourself and register it:

```go
p := etcd.New(etcd.WithEndpoints("etcd-a:2379", "etcd-b:2379"))
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom `*clientv3.Client` (TLS, username/password auth, custom
dial options, ...):

```go
client, _ := clientv3.New(clientv3.Config{
    Endpoints: []string{"etcd:2379"},
    Username:  "app",
    Password:  os.Getenv("ETCD_PASSWORD"),
    TLS:       tlsConfig,
})
p := etcd.New(etcd.WithClient(client))
```

### Options

| Option | Effect |
| --- | --- |
| `WithEndpoints(eps...)` | Set the etcd endpoints (overrides `ETCD_ENDPOINTS`) |
| `WithClient(*clientv3.Client)` | Inject a pre-configured etcd client (custom TLS/auth/dial) |

## Native watch (watch stream)

The provider implements `mamori.WatchableProvider` using **etcd's native watch
stream** - the idiomatic etcd push mechanism, not a polling ticker:

1. On `Watch`, the provider calls `clientv3.Watcher.Watch(ctx, key)`.
2. etcd streams a `WatchResponse` the instant the key changes. For every `PUT`
   event the provider emits an `Update` carrying the new value bytes and the
   event's `ModRevision` as `Value.Version`. Delete events are ignored.
3. Transient watch errors (e.g. a compaction that cancels the stream) are
   delivered as `Update{Err: ...}`.
4. When the watch context is cancelled etcd closes the watch channel, the
   goroutine exits, and the Update channel is closed - no goroutine leaks
   (verified with `goleak`).

Because etcd watches deliver events from the current revision onward (not the
current value), no baseline `Update` is emitted at subscription time; mamori
already holds the value from the initial `Resolve`.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake client with etcd revision + watch-stream semantics (`go test ./...`) |
| Resolve, JSON `#key` selection, not-found, version monotonicity, context cancellation | **Verified** (unit tests) |
| Native watch (change delivery + cancel/close, no goroutine leak) | **Verified** against the fake (including `-race`) |
| Endpoint parsing (`ETCD_ENDPOINTS`), missing-endpoints error | **Verified** (unit tests) |
| End-to-end against a real etcd server | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces etcd's
`ModRevision` bump-on-write and PUT-event watch stream, so `go test ./...`
requires **no** running etcd.

### Live integration test

An integration test exercises a real etcd server. It is guarded by a build tag
and skips unless `ETCD_ENDPOINTS` is set:

```sh
etcd &
export ETCD_ENDPOINTS=127.0.0.1:2379
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/etcd
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
