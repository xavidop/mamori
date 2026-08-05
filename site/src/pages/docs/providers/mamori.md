---
layout: ../../../layouts/DocsLayout.astro
title: mamori (client) provider
---

# mamori (client)

The `mamori://` provider is the client half of the [config server](/docs/server/): it resolves **binding names** against a running `server.Server`, over that server's v1 HTTP wire protocol, instead of resolving a secret-manager ref directly.

A `mamori://` ref never names an upstream ref. `mamori://db-password` asks the config server "give me the binding called `db-password`," and the server answers with whatever value that binding currently resolves to on its own end, whether that is `vault://secret/data/db#password`, `aws-sm://prod/db`, or another mamori server three hops away. The consuming struct has no way to tell any of that apart: it sees a name go out and a value come back, exactly as it would for any other provider.

| | |
| --- | --- |
| Scheme | `mamori://` |
| Module | `github.com/xavidop/mamori/providers/mamori` (package `mamoriprov`) |
| Sensitive | passthrough (the server's `Sensitive` bit survives the hop) |
| Watch | **native** (Server-Sent Events, with reconnect) |
| Errors | passthrough (the real upstream kind, not a generic proxy failure) |

## Install

```bash
go get github.com/xavidop/mamori/providers/mamori
```

```go
import (
	"github.com/xavidop/mamori"
	mamoriprov "github.com/xavidop/mamori/providers/mamori"
)

cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(mamoriprov.New(mamoriprov.Config{
		Endpoint: "unix:///run/mamori.sock",
	})),
)
```

This provider's own package is already named `mamoriprov`, not `mamori`, precisely so that code importing both the core `github.com/xavidop/mamori` package and this provider never has to alias either one to avoid a collision. The `mamoriprov` alias on the import line above is therefore not compiler-required (Go already resolves identifiers by the package's declared name), but every example on this page writes it anyway, the same way the package's own doc comment does, so the two `mamori`-named things stay visually distinct at every call site.

Unlike most providers, there is nothing to register with a blank import: a `mamori://` binding name only means something relative to one specific config server's `Endpoint`, so this provider is always constructed explicitly with `mamoriprov.New` and passed via `mamori.WithProvider`.

## Using the ref

```text
mamori://<binding-name>
```

`mamori://db-password` resolves the binding `db-password`. The path is a binding name the server operator declared with `server.Bind`/`server.BindFile` (see the config server's [Bindings section](/docs/server/bindings/)); it is never, and can never be, an upstream ref supplied by this side. That asymmetry is the whole point of decision D9: a client that could instead send its own ref would let the config server's credentials be used to reach anything the client asked for, not just the bindings the operator chose to expose.

```go
type Config struct {
	DBPassword secret.String `source:"mamori://db-password"`
	APIKey     secret.String `source:"mamori://api-key"`
}
```

A struct with several `mamori://` fields, like `Config` above, does not make one round trip per field. See [Watch](#watch) below for how `Load`/`Watch`'s batch path collapses them into a single request.

## Explicit configuration

### Endpoint forms

`Config.Endpoint` accepts exactly three forms:

| Form | Example | Notes |
| --- | --- | --- |
| `unix://` | `unix:///run/mamori.sock` | Dials the Unix domain socket at the given path with a custom dialer; no TLS involved. |
| `https://` | `https://config.internal:8443` | Standard TLS. Attach `Config.TLSConfig` for custom roots, or `TLSConfig.Certificates` to present a client certificate for mTLS. |
| `http://` | `http://config.internal:8080` | Refused unless `Config.InsecureNoTLS` is `true`. |

`InsecureNoTLS` is named to be uncomfortable to type and read, on purpose, the same way the config server's own `server.InsecureNoTLS()` transport option is: `grep -r InsecureNoTLS` finds every place a client was pointed at a plaintext endpoint, which should never be an accident. Any other scheme, or an empty `Endpoint`, is rejected with an error satisfying `errors.Is(err, mamori.ErrInvalid)`.

```go
mamoriprov.New(mamoriprov.Config{
	Endpoint: "https://config.internal:8443",
	TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{clientCert}, // mTLS
	},
})
```

`New` never fails outright, matching how every other provider registers a zero-config default from `init`: a malformed or empty `Endpoint` is recorded on the returned `*Provider` and surfaced (wrapped as `mamori.ErrInvalid`) from the first `Resolve`, `ResolveBatch`, or `Watch` call instead.

### Several replicas

When the config server runs as [several replicas](/docs/server/ha/), give the client the list instead of a single endpoint. `Config.Endpoints` replaces `Config.Endpoint` and takes the same three forms, in failover order:

```go
prov, err := mamoriprov.New(mamoriprov.Config{
	Endpoints: []string{
		"https://mamori-0.internal:8443",
		"https://mamori-1.internal:8443",
		"https://mamori-2.internal:8443",
	},
})
```

Each entry gets its own transport, so the list may mix forms, for example a local `unix://` replica tried first with TCP replicas behind it.

**When it fails over.** `Resolve` and `ResolveBatch` walk the list until a replica answers, moving on only when a replica is unreachable or broken: a refused dial, a TLS failure, a timeout, `unavailable`, or an unclassifiable `5xx`.

**When it does not.** An *authoritative* answer, one every replica would give alike, is returned immediately rather than replayed against the rest: `not_found`, `permission_denied`, `unauthenticated`, `invalid`, and `rate_limited`. Retrying those would turn one clean `403` into one request per replica, and in the `rate_limited` case would amplify exactly the load a throttle exists to shed. If every replica fails, the last error is returned with its kind intact, so `errors.Is(err, mamori.ErrUnavailable)` still holds.

**Watch** rotates to the next endpoint on each reconnect, so a replica that dies mid-stream cannot black-hole the watch. The backoff sleep applies only after a full cycle through the list, never between one replica and the next, so a three-replica deployment fails over in three quick dials rather than sleeping in between.

Setting both `Endpoint` and `Endpoints`, or a malformed entry anywhere in the list, is a configuration error surfaced from every call. Neither is quietly ignored: an operator who mistyped one of three replicas should find out, not run on two believing they have three. A single-element `Endpoints` behaves exactly like the equivalent `Endpoint`, one request and no retries.

### Client credentials

The config server authenticates inbound requests with a `mamori.Authenticator` (see [Auth](/docs/auth/)), which is a server-side concept: it authenticates a request that has already arrived. This provider does not reuse `mamori.Authenticator` for the reverse direction, because attaching a credential to an outbound request is a different shape entirely. Instead:

```go
mamoriprov.New(mamoriprov.Config{Endpoint: "https://config.internal:8443"},
	mamoriprov.WithHeader("Authorization", "Bearer "+token),
)
```

- **`WithHeader(key, value string)`** sets one static header on every outbound request. It covers `mamori.BearerToken`, `mamori.APIKey`, and `mamori.BasicAuth` on the server side.
- **`WithRequestEditor(fn func(*http.Request))`** is the general form `WithHeader` is built on, for anything more dynamic than a static header (a token refreshed per request, a signed header, and so on).
- **mTLS** is configured entirely through `Config.TLSConfig.Certificates`, matching the server's `mamori.MTLS` authenticator.
- **`PeerCred`** needs no client-side configuration at all: over a Unix socket, the kernel supplies the connecting process's uid/gid to the server directly (`SO_PEERCRED`/`LOCAL_PEERCRED`), so there is nothing for this provider to attach.

`Close()` is idempotent and terminal: after it returns, every `Resolve`, every `ResolveBatch`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting any replica. It returns every endpoint's idle HTTP connections to the pool. Unlike most HTTP-backed providers here, this happens in the default configuration too: an endpoint built without `Config.HTTPClient` gets its own real `*http.Transport`, which belongs to this provider alone. A client supplied through `Config.HTTPClient` is never closed or invalidated, only its idle connections are released, and only when that client's `Transport` is non-nil.

A `Watch` that was **already running** when `Close` was called is a different case and is **not** covered by that guarantee: `Close` does not end it, and the SSE connection it is currently streaming on is not one of the idle connections `Close` returns to the pool, so it keeps delivering live updates. The closed check is reached only on the next reconnect, and from there the watch degrades to an error stream carrying `errors.Is(err, mamori.ErrUnavailable)`, retried with backoff, until that watch's own context is cancelled. Cancelling that context is the only reliable way to stop it. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for what every other provider does here.

## Watch

`mamori://` implements `mamori.WatchableProvider` as a **native** watch: mamori must never wrap it in its own polling adapter. `Watch` opens a persistent `GET /v1/watch?name=<binding>` Server-Sent Events connection and forwards every `update`/`error` frame the server sends as a `mamori.Update`.

The connection is not assumed to stay up forever. The server drops the SSE stream on shutdown (and idle proxies, restarts, and ordinary network hiccups can too), so the client reconnects and resubscribes automatically, with exponential backoff and jitter between attempts, capped so a persistently unreachable server is retried at most every 30 seconds. Backoff resets to its floor as soon as a stream is successfully re-established, regardless of how long it then stays up. None of this is visible on the `<-chan mamori.Update` the caller reads from: a transient disconnect-and-reconnect is not itself delivered as an error, only a genuine classified failure from the server is (see [Error classification](#error-classification) below).

A struct with several `mamori://` fields does not open several connections, either for a one-shot resolve or for `ResolveBatch`: `mamori.Load`/`mamori.Watch` group same-provider refs and this provider answers a batch with a single `POST /v1/values` request carrying every binding name at once, rather than one request per field.

### Going backwards in time is prevented

`Watch` reconnects on its own, and across [several replicas](#several-replicas) it rotates endpoints, so a reconnect can land on a replica whose copy of a binding is older. Left alone, that older value would arrive looking like a brand new change.

It does not. The client tracks the newest `resolved_at` it has delivered for each binding and drops an update older than that watermark. The watermark is per binding name and **survives reconnects**, which is the whole point: the reconnect is exactly when the risk appears.

Three deliberate details:

- An update carrying no `resolved_at` is always forwarded, so an older server that does not send the field behaves exactly as before.
- Error frames are never dropped. An error describes the connection right now and cannot be out of order.
- Comparison is against wall clocks on different machines, so a one second tolerance absorbs ordinary clock skew. Replica lag is tens of seconds (the upstream poll interval) while NTP-synced hosts sit within milliseconds, so the band is far below the lag it is catching, and ties go to delivering the value rather than withholding it.

## Error classification

Classification is a full passthrough, not just a "some backend, some error" mapping: the wire `kind` the config server reports is exactly the `mamori.Kind` its own upstream provider produced, and this client reconstructs the matching sentinel from it. Concretely:

- `errors.Is(err, mamori.ErrPermissionDenied)` holds against a `mamori://` resolve exactly when it would hold resolving the real backend ref directly, through the config server hop, because the server's own classification (see the config server's [wire protocol section](/docs/server/wire-protocol/)) never gets flattened into something generic on the way back.
- `mamori.Doctor` and a running watcher's `Status`/`Health` report the real upstream kind in `FieldStatus.LastKind`, not a generic "the proxy failed" kind. A `vault://` binding whose Vault lease was revoked reports `permission_denied` through `mamori://` exactly as it would resolving `vault://` directly.
- A wire `not_found` (an unbound name, or the server's own upstream reporting one) maps to `mamori.ErrNotFound`, so a struct field's default value or `Optional` tag still applies exactly as it would for any other provider.
- `mamori.Value.Sensitive` survives the hop unchanged: a binding backed by a secret-manager ref that sets `Sensitive` keeps it set on this side, so `secret.String`/`secret.Bytes` redaction, audit logging, and every other sensitivity-aware behavior downstream of `Load`/`Watch` keeps working exactly as if the field had resolved directly against the upstream provider.

A batch (`ResolveBatch`) treats a hard per-name classified error (anything other than `not_found`) as a whole-call failure rather than silently dropping that one entry, since dropping it would let a struct field fall back to its zero value in place of a secret the caller was denied, or one that is genuinely unavailable - not something a per-ref `Resolve` of that same name would ever do either.

Across replicas, a value response also carries `resolved_at` (when that replica last fetched the bytes from upstream) and `stale` (it is serving last-known-good while upstream fails). Because each replica watches on its own schedule, these are what let a client tell that it reached a replica running behind one it already spoke to. See [High availability](/docs/server/ha/).

See the config server's page for the full [`kind` field semantics](/docs/server/wire-protocol/#read-kind-on-a-success-fresh-vs-stale-but-serving), including the distinction between a failure `kind` and a successful-but-stale `kind` on a value that is still being served while its upstream is currently failing (this provider returns that stale value with a nil error, exactly as the server intends).
