# mamori config-server client (`providers/mamori`)

`github.com/xavidop/mamori/providers/mamori` (package `mamoriprov`) is a `mamori://` provider that resolves **binding names**, not upstream refs, against a running [config server](../../server) over its v1 HTTP wire protocol. `mamori://db-password` asks the server "give me the binding called `db-password`"; the server resolves that binding through whatever provider it is actually backed by (Vault, AWS Secrets Manager, another mamori server) and hands back only the value, so this side never knows or needs to know what is on the other end.

## Minimal usage

```go
import (
	"github.com/xavidop/mamori"
	mamoriprov "github.com/xavidop/mamori/providers/mamori"
)

type Config struct {
	DBPassword secret.String `source:"mamori://db-password"`
}

cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(mamoriprov.New(mamoriprov.Config{
		Endpoint: "unix:///run/mamori.sock",
	})),
)
```

There is no zero-config default to register with a blank import: a binding name only means something relative to one specific server, so `mamoriprov.New` is always called explicitly and passed via `mamori.WithProvider`.

## Endpoint forms

`Config.Endpoint` accepts exactly three forms:

- `unix:///path/to.sock` - dials the Unix domain socket at that path.
- `https://host:port` - standard TLS; set `Config.TLSConfig.Certificates` for mTLS.
- `http://host:port` - refused unless `Config.InsecureNoTLS` is `true`, named to be uncomfortable on purpose (mirrors the server's own `InsecureNoTLS` option).

## Several replicas (HA)

A config server running as N replicas is configured with `Config.Endpoints` **instead of** `Config.Endpoint`, an ordered list of those same three forms:

```go
mamoriprov.New(mamoriprov.Config{
	Endpoints: []string{
		"https://mamori-0.internal:8443",
		"https://mamori-1.internal:8443",
		"https://mamori-2.internal:8443",
	},
})
```

The order is the failover order, and each entry gets its own transport, so the list may mix forms (a local `unix://` replica first, TCP replicas behind it).

- `Resolve` and `ResolveBatch` walk the list until a replica answers. They move on only when a replica is **unreachable or broken** (refused dial, TLS failure, timeout, `unavailable`, or an unclassifiable 5xx). An **authoritative** answer that every replica would give alike (`not_found`, `permission_denied`, `unauthenticated`, `invalid`, `rate_limited`) is returned immediately, so one clean 403 stays one request rather than becoming one per replica. When every replica fails, the last error is returned with its kind intact, so `errors.Is(err, mamori.ErrUnavailable)` still holds.
- `Watch` rotates to the next endpoint on every reconnect, so a replica that dies mid-watch cannot black-hole the stream. The backoff sleep is applied only after a full cycle through the list, never between one replica and the next: a 3-replica deployment fails over in three quick dials instead of sleeping in between.
- Because each replica watches upstream on its own schedule, that rotation can land on a replica that is a poll cycle behind. `Watch` therefore remembers the newest `resolved_at` it has **delivered** per binding name and drops an update dated meaningfully before it, so a reconnect can never re-announce an older value as a fresh change. The watermark survives reconnects (that is the entire point), only strictly older values are dropped, error updates are never dropped, and a server that does not send `resolved_at` is forwarded unconditionally. Comparing timestamps across replicas assumes their clocks are roughly in step; see `freshness.go` for that assumption and the small skew tolerance that absorbs it.
- Setting **both** `Endpoint` and `Endpoints` is a configuration error surfaced from every call, as is a malformed entry anywhere in the list. Neither is quietly ignored: an operator who typo'd one of three replicas must find out rather than run on two while believing they have three.

Setting a single-element `Endpoints` behaves exactly like the equivalent `Endpoint`: one request, no retries.

## What this provider is (and is not)

- It implements `mamori.Provider`, `mamori.BatchProvider` (one `POST /v1/values` request for a struct with several `mamori://` fields, not one per field), and `mamori.WatchableProvider` as a **native** watch: a persistent `GET /v1/watch` Server-Sent Events stream, with automatic reconnect, resubscription, and backoff-with-jitter if the connection drops.
- Error classification is a **passthrough**, not a fixed mapping: the `kind` the client reconstructs from the wire is exactly the `mamori.Kind` the server's own upstream provider reported, so `errors.Is(err, mamori.ErrPermissionDenied)` holds through the hop the same as it would resolving the backend ref directly, and a wire `not_found` still maps to `mamori.ErrNotFound` so defaults and optional fields keep working.
- `Value.Sensitive` also survives the hop unchanged, so redaction downstream of `Load`/`Watch` keeps working exactly as if the field had resolved against the upstream provider directly.
- Client credentials are attached with `mamoriprov.WithHeader`/`mamoriprov.WithRequestEditor` (Bearer/API key/basic auth), or `Config.TLSConfig.Certificates` for mTLS. This package deliberately does not reuse `mamori.Authenticator`, since that authenticates an inbound request on the server side, a different shape from attaching a credential to an outbound one. `PeerCred` needs no client-side configuration; the kernel supplies the credential over the Unix socket.

## Documentation

- 📖 **Full docs for this provider:** https://mamorigo.dev/docs/providers/mamori
- 🖧 **The config server this client talks to:** https://mamorigo.dev/docs/server
- 🔁 **Running that server as several replicas** (readiness gating, draining, and the `resolved_at`/`stale` freshness fields this client can compare): https://mamorigo.dev/docs/server/ha

`Close()` is idempotent and terminal: after it returns, every `Resolve`,
`ResolveBatch`, and `Watch` report `errors.Is(err, mamori.ErrUnavailable)`
locally, without contacting any replica. It returns every endpoint's idle HTTP
connections to the pool. Unlike most HTTP-backed providers here, this happens
in the default configuration too: an endpoint built without `Config.HTTPClient`
gets its own real `*http.Transport`, which belongs to this provider alone. A
client supplied through `Config.HTTPClient` is never closed or invalidated,
only its idle connections are released, and only when that client's
`Transport` is non-nil.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/mamori
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
