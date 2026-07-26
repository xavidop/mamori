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

## What this provider is (and is not)

- It implements `mamori.Provider`, `mamori.BatchProvider` (one `POST /v1/values` request for a struct with several `mamori://` fields, not one per field), and `mamori.WatchableProvider` as a **native** watch: a persistent `GET /v1/watch` Server-Sent Events stream, with automatic reconnect, resubscription, and backoff-with-jitter if the connection drops.
- Error classification is a **passthrough**, not a fixed mapping: the `kind` the client reconstructs from the wire is exactly the `mamori.Kind` the server's own upstream provider reported, so `errors.Is(err, mamori.ErrPermissionDenied)` holds through the hop the same as it would resolving the backend ref directly, and a wire `not_found` still maps to `mamori.ErrNotFound` so defaults and optional fields keep working.
- `Value.Sensitive` also survives the hop unchanged, so redaction downstream of `Load`/`Watch` keeps working exactly as if the field had resolved against the upstream provider directly.
- Client credentials are attached with `mamoriprov.WithHeader`/`mamoriprov.WithRequestEditor` (Bearer/API key/basic auth), or `Config.TLSConfig.Certificates` for mTLS. This package deliberately does not reuse `mamori.Authenticator`, since that authenticates an inbound request on the server side, a different shape from attaching a credential to an outbound one. `PeerCred` needs no client-side configuration; the kernel supplies the credential over the Unix socket.

## Documentation

- 📖 **Full docs for this provider:** https://mamorigo.dev/docs/providers/mamori
- 🖧 **The config server this client talks to:** https://mamorigo.dev/docs/server

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/mamori
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
