# mamori - HashiCorp Consul KV provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from the [HashiCorp Consul](https://www.consul.io/) KV store, with **native
hot-reload** driven by Consul blocking queries.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/consul"
```

Importing the package registers the `consul` scheme with mamori. The Consul client
is built lazily on first use from the ambient environment, so importing the package
never contacts Consul.

## Scheme

```
consul://<kv-path>[#json-key]
```

- `<kv-path>` - the Consul KV key path, e.g. `config/app/db`. No leading slash.
- `#json-key` - optional. When present, the stored value is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same
  behavior as every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `consul://config/app/log_level` | Raw value of key `config/app/log_level` |
| `consul://config/app/db#password` | Field `password` of the JSON object at `config/app/db` |
| `consul://features/flags#dark_mode` | Field `dark_mode` of the JSON object at `features/flags` |

```go
type Config struct {
    LogLevel   string `source:"consul://config/app/log_level"`
    DBHost     string `source:"consul://config/app/db#host"`
    DBPassword string `source:"consul://config/app/db#password"`
    DarkMode   bool   `source:"consul://features/flags#dark_mode"`
}
```

### Value semantics

- `Value.Bytes` - the raw KV entry value (`KVPair.Value`).
- `Value.Version` - the entry's `ModifyIndex` (decimal string). This is Consul's
  native revision, so change detection is exact and cheap - no byte comparison.
- `Value.Sensitive` - always `false`. Consul KV stores configuration, not managed
  secrets. Wrap a field in `secret.String` if you want redaction anyway.
- A missing key returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

By default the provider uses Consul's standard environment variables via
`api.DefaultConfig()`:

| Variable | Purpose |
| --- | --- |
| `CONSUL_HTTP_ADDR` | Agent address, e.g. `127.0.0.1:8500` (default) or `https://consul.example.com` |
| `CONSUL_HTTP_TOKEN` | ACL token |
| `CONSUL_HTTP_SSL`, `CONSUL_CACERT`, `CONSUL_CLIENT_CERT`, `CONSUL_CLIENT_KEY` | TLS |
| `CONSUL_NAMESPACE` | Consul Enterprise namespace |

For explicit configuration, construct the provider yourself and register it:

```go
p := consul.New(
    consul.WithAddress("https://consul.example.com"),
    consul.WithToken(os.Getenv("MY_CONSUL_TOKEN")),
    consul.WithWaitTime(2*time.Minute), // max blocking-query duration
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom Consul client (custom TLS, datacenter, HTTP client):

```go
client, _ := api.NewClient(&api.Config{ /* ... */ })
p := consul.New(consul.WithClient(client))
```

### Options

| Option | Effect |
| --- | --- |
| `WithAddress(addr)` | Override the Consul HTTP address |
| `WithToken(tok)` | Set the ACL token |
| `WithWaitTime(d)` | Max server-side blocking-query duration (default 5m) |
| `WithClient(*api.Client)` | Inject a pre-configured Consul client |

## Native watch (blocking queries)

The provider implements `mamori.WatchableProvider` using **Consul blocking
queries** - the idiomatic Consul push mechanism, not a polling ticker:

1. On `Watch`, the current value is emitted immediately as a baseline.
2. The provider then calls `KV.Get` with `QueryOptions{WaitIndex: <last ModifyIndex>, WaitTime: <wait time>}`.
   Consul holds the request open until the key's `ModifyIndex` advances past
   `WaitIndex` (or `WaitTime` elapses), so a change is delivered the instant it
   happens with no busy polling.
3. Each observed change is emitted as an `Update`. Transient errors are delivered
   as `Update{Err: ...}` and the loop retries after a short backoff.
4. When the watch context is cancelled the in-flight request is aborted, the
   goroutine exits, and the channel is closed - no goroutine leaks (verified with
   `goleak`).

`WithWaitTime` bounds how long a single request blocks; it does **not** affect how
quickly a change is observed (immediate) or how quickly the watch reacts to context
cancellation (immediate).

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake KV with blocking-query semantics (`go test ./...`) |
| Resolve, JSON `#key` selection, not-found, version monotonicity, context cancellation | **Verified** (unit tests) |
| Native blocking-query watch (baseline + change + cancel/close, no goroutine leak) | **Verified** against the fake |
| End-to-end against a real Consul agent | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces Consul's
`ModifyIndex` bump-on-write and blocking-query behavior, so `go test ./...`
requires **no** running Consul.

### Live integration test

An integration test exercises a real Consul agent. It is guarded by a build tag
and skips unless `CONSUL_HTTP_ADDR` is set:

```sh
consul agent -dev &
export CONSUL_HTTP_ADDR=127.0.0.1:8500
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/consul
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
