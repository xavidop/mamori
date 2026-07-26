---
layout: ../../layouts/DocsLayout.astro
title: Config server
---

# Config server

`server` (import path `github.com/xavidop/mamori/server`) is a separate module: a process built around `mamori.Watch`-style resolution that fronts a fixed, operator-declared table of name-to-ref bindings and serves the resolved values to many local or networked callers over a small HTTP wire protocol. Where `Load`/`Watch` and `WithAdminHTTP` (see [Observability](../observability)) run *inside* the application that needs the config, the config server is a standalone deployment: one process holds the credentials, everyone else asks it for values by name.

Read this page before deploying one. It is deliberately not a sales pitch: the last major section is a plain statement of what running this concentrates and why that is a real cost, not just a caveat.

## Why fan out through a server at all

Three motivations, all of the same shape: fewer places hold real credentials, not more.

- **Credentials in one place, not N.** Every workload that would otherwise need its own AWS/Vault/GCP credential to resolve `aws-sm://prod/db#password` directly instead needs only a credential (or a Unix socket) that reaches the config server. The blast radius of a compromised workload shrinks to whatever bindings its Policy grants it, not the underlying backend credential itself.
- **N consumers, one upstream watch.** Ten replicas of a service watching `vault://secret/data/db` directly means ten independent lease renewals and ten independent poll loops hitting Vault. A config server resolves that ref exactly once (one `mamori.WatchRef` per binding - see [How a binding resolves](#how-a-binding-resolves) below) and fans the result out to as many local readers as connect, the same way an `http.Server` fans one listening socket out to many requests.
- **Workloads without their own IAM identity.** A batch job, a short-lived container, or a language/runtime with no good story for assuming an IAM role can still get a secret: it authenticates to the config server (a bearer token, a Unix socket it was handed) instead of to the cloud provider directly.

None of this is free. See [Blast radius](#blast-radius) for the cost side of the same coin.

## How it fits together

```go
srv, err := server.New(
	server.Bind("db-password", "vault://secret/data/db#password"),
	server.Bind("api-key", "aws-sm://prod/api-key"),
	server.WithProvider(vaultProvider),
	server.WithProvider(awsProvider),
	server.WithPolicy(server.PrefixPolicy(map[string][]string{
		"svc-orders": {"db-password"},
		"svc-billing": {"api-key", "db-password"},
	})),
	server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),
	server.Unix("/run/mamori/server.sock", 0600),
	server.WithAudit(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}
defer srv.Close()

if err := srv.Serve(ctx); err != nil {
	log.Fatal(err)
}
```

- **Construction:** `server.New(opts ...server.Option) (*server.Server, error)`. `New` validates every security-critical invariant (a `Policy` is configured, an `Authenticator` is configured or `NoAuth()` was explicit, `NoAuth()` is never combined with a TCP listener, bindings parse and their scheme gates pass) before returning a `*Server`. It starts nothing yet: no listener is bound, no upstream watch runs.
- **Lifecycle:** `(*Server).Serve(ctx context.Context) error` begins upstream watching and binds and runs every configured transport; it blocks until a listener fails or the server is closed. `(*Server).Close() error` is idempotent and safe to call even if `Serve` never ran (defer it unconditionally right after `New`). `(*Server).Addrs() []net.Addr` reports every bound listener's address, most useful for discovering the OS-chosen port after binding to `:0`. `(*Server).Handler() http.Handler` returns the v1 wire protocol handler on its own, for a caller who wants to mount it on a listener of their own rather than use `Unix`/`TCP`.

The rest of this page works through each piece: bindings, providers, policy, authentication, transports, the wire protocol, and audit.

## Bindings: the only thing a client can name

```go
func Bind(name, ref string) Option
func BindFile(path string) Option
```

A binding maps a name (what a client requests) to a ref (what actually gets resolved), exactly as a struct field's `source` tag does for `Load`/`Watch`. `Bind` declares one inline; `BindFile` reads a YAML file:

```yaml
bindings:
  db-password: vault://secret/data/db#password
  api-key: aws-sm://prod/api-key
```

**This is the whole security model in one sentence: a client sends a name, never a ref.** `Bind`/`BindFile` are the *only* way a binding comes into existence, and they are called by the operator writing the server's startup code, never derived from anything a request carries. If a client could instead supply its own ref, `file:///etc/shadow` and an `exec:` command of the client's choosing would be reachable through whatever credentials this process holds - the fixed binding table deletes that entire class of vulnerability by construction, not by a check that a handler could forget to make.

A duplicate binding name fails `New` outright (no last-write-wins shadowing), and both `Bind` and `BindFile` declarations are validated together in one pass after every `Option` has applied, so declaration order relative to `AllowExec`/`AllowChaining` below never matters.

### The `exec:` and `mamori:` gates

Two ref schemes are rejected at construction unless explicitly allowed:

```go
func AllowExec() Option
func AllowChaining() Option
```

- **`exec:`** runs an arbitrary command on the server's host. `AllowExec()` lifts `New`'s construction-time gate, but core's `exec:` provider has no exported constructor, so making an `exec:` binding actually resolve also requires the operator to supply their own exec `Provider` via `WithProvider` - two separate, deliberate steps for a scheme that means "remote command execution reachable by every authorized consumer."
- **`mamori:`** chains to another config server. `AllowChaining()` lifts the gate; without it, a `mamori:` binding could quietly wire up a cycle (a server that, directly or transitively, points back at itself).

Every other scheme (`env:`, `aws-sm:`, `vault:`, `file:`, `gcp-sm:`, and so on) needs neither option - only these two schemes carry consequences serious enough to require an explicit opt-in.

## How a binding resolves

The server holds no polling loop of its own. Each binding is watched exactly once, through the same `mamori.WatchRef` machinery `Watch[T]` uses internally, fanned out to however many concurrent readers ask for it:

```go
func WithProvider(p mamori.Provider) Option
```

`WithProvider` registers a `Provider` for its own `Scheme()`, and a binding resolves through whichever `Provider` is registered for its ref's scheme. **There is no global-registry fallback here.** Core's `Load`/`Watch` will fall back to `mamori.Register`'s process-wide registry when no explicit provider is given for a scheme; that fallback is implemented by an unexported function private to the core package, so this module - deliberately - has no way to reach it. Every provider a binding can resolve through is one the operator named explicitly with `WithProvider`, not whatever happened to self-register via some imported package's `init()`. A binding whose scheme has no registered provider is not a construction-time error: it starts cleanly, and `lookup` on it simply reports a resolve error while every other binding continues resolving normally.

A binding that has resolved successfully at least once keeps serving that **last-known-good value** if its upstream subsequently starts failing - the same "an update that fails validation never overwrites `Get()`" spirit `Load`/`Watch` apply to a whole config, applied per-binding here. See [The `kind` field: fresh vs. stale-but-serving](#the-kind-field-fresh-vs-stale-but-serving) for how that state surfaces on the wire.

## Authorization: the mandatory `Policy`

```go
type Policy interface {
	Allow(id mamori.Identity, name string) error
}

type PolicyFunc func(id mamori.Identity, name string) error
```

`WithPolicy(p Policy) Option` is mandatory. `New` refuses to construct a `Server` with no `Policy` configured - there is no implicit default, not even deny-all, because a silent default is either too permissive to be safe or too restrictive to be usable, and either way it hides a decision the operator must make and see themselves make.

`PolicyFunc` adapts a plain function, the same pattern as `mamori.AuthFunc` for `Authenticator`. Two constructors ship:

```go
func AllowAll() Policy
func PrefixPolicy(rules map[string][]string) Policy
```

- **`AllowAll()`** permits every identity access to every binding. It exists for a trusted-sidecar deployment (a process running as the sole consumer, or one where access control is already fully enforced upstream) where per-name rules would add configuration without adding security. Because `New` refuses to start with no `Policy` at all, choosing `AllowAll()` is always an explicit, greppable line in the operator's own code, never a fallback nobody decided on.
- **`PrefixPolicy(rules)`** grants access by subject: `rules[id.Subject]` is a list of `path.Match` globs checked against the requested binding name (`*` and `?` do not cross a `/`, `[...]`/`[^...]` character classes work as in a shell glob). A subject with no entry in `rules` is denied, with no fallback default-allow.

**A denied name is indistinguishable from a nonexistent one.** `Policy.Allow` is handed only an `Identity` and a name string - it has no visibility into the binding table - so it structurally cannot leak existence through its own return value, and the wire handler completes that guarantee by always answering a denial with the same `403 permission_denied`, with the same message, whether or not the name is a real binding. Authorization runs before the binding table is ever consulted for a given request, so `Policy` can never become a way to enumerate what a server is configured to serve.

## Authentication

```go
func WithAuth(a mamori.Authenticator) Option
func NoAuth() Option
```

The config server reuses core's `Authenticator`/`Identity` unchanged (see [Auth](../auth)): `BasicAuth`, `BearerToken`, `APIKey`, `MTLS`, `PeerCred` (below), and their `AnyOf`/`AllOf` composition all work here exactly as they do against the admin HTTP endpoint, because both surfaces share the one interface.

Authentication is mandatory, the same way `Policy` is: `New` refuses to construct a `Server` with neither `WithAuth` nor `NoAuth()` called. `NoAuth()` exists for a Unix-socket-only deployment where the filesystem (and, with `PeerCred`, the kernel) already bounds who can connect - but **`NoAuth()` is refused on a TCP listener.** A Unix socket is reachable only by processes on the same host; TCP has no such boundary, so serving every configured binding to anonymous TCP callers would defeat the entire point of running a credential-holding server in the first place. `New` (not `Serve`) rejects this combination, so the mistake surfaces before any port is ever bound, let alone reachable.

### `PeerCred`: authenticating a Unix-socket peer by its kernel-verified identity

`PeerCred` (documented in full on the [Auth](../auth#peercred) page) is the strongest option for a Unix-socket deployment: it authenticates a connecting process by the uid/gid the kernel itself reports at accept time (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on Darwin), which cannot be spoofed by anything the client presents in the request. It requires this server's Unix transport specifically - the plumbing that reads a connection's peer credentials and stashes them where `PeerCred.Authenticate` can find them is wired up by `Unix(...)` (see [Transports](#transports) below), not by an arbitrary `http.Handler` mount.

## Transports

```go
func Unix(path string, mode os.FileMode) Option
func TCP(addr string, opts ...TCPOption) Option
func TLS(cfg *tls.Config) TCPOption
func InsecureNoTLS() TCPOption
```

`Unix(path, mode)` binds a Unix-domain-socket listener at `path`, with permission bits `mode` (`0600`, owner read/write only, is recommended for most deployments - anyone who can connect to the socket can request any binding this server's `Policy` allows them). Every connection accepted on a `Unix` listener has its kernel-verified peer credentials captured via `http.Server.ConnContext` and made available to a `PeerCred` `Authenticator`, completing the seam described above.

`TCP(addr, opts...)` binds a TCP listener. **TCP without TLS fails construction** unless `InsecureNoTLS()` is given explicitly:

```go
server.TCP(":8443", server.TLS(tlsConfig))       // ordinary case
server.TCP(":8080", server.InsecureNoTLS())      // deliberate, greppable opt-out
```

`InsecureNoTLS`'s name is deliberately uncomfortable: `grep -r InsecureNoTLS` finds every place an operator chose to serve this server's bindings over plaintext TCP, which should never be an accident. Prefer `TLS(cfg)` in any deployment that is not already fully isolated at the network layer.

Both transports can run at once, under the same `Policy` and the same wire handler: call `Unix` and `TCP` any number of times, in any order, and `Serve` binds every one of them (every `Unix` listener before every `TCP` listener) before it starts accepting connections on any of them.

## The v1 wire protocol

The handler `Server.Handler()` returns (and that `Serve` mounts on every configured transport) exposes four routes:

| Route | Purpose |
| --- | --- |
| `GET /v1/values/{name}` | Resolve one binding by name |
| `POST /v1/values` | Resolve a batch: body `{"names":[...]}` |
| `GET /v1/watch` | Subscribe to one or more bindings over Server-Sent Events |
| `GET /v1/healthz` | Bare liveness check |

**`/v1/` freezes the moment anything outside your control speaks it.** This is a versioned wire protocol, not an internal implementation detail: once an external client depends on today's route shapes and JSON fields, they cannot change incompatibly. Treat every field below as a stable contract, not a convenience that can be tweaked later.

Every value-bearing response carries `Cache-Control: no-store` - nothing that could hand back a secret is ever cacheable by an intermediary.

### Request ordering

Every route except `/v1/healthz` runs, in this exact order, on every request:

1. **Authenticate** (`mamori.Authenticator.Authenticate`, or skipped entirely in `NoAuth` mode). Failure is `401`, with `WWW-Authenticate` set if the `Authenticator` implements `Challenger`.
2. **Authorize the specific name being requested** (`Policy.Allow`), separately for every name in a batch or a watch subscription. Failure is `403`.
3. **Only then** read the binding.

A denied caller gets `403` for a requested name whether or not it is a real binding - authorization runs, and fails closed, before the binding table is ever consulted, so `Policy` can never be used to enumerate what this server serves.

`GET /v1/healthz` is the one exception: it never calls `Authenticate` or `Policy.Allow`, and its response never names a binding, so a liveness probe with no credential at all can never be turned into a way to learn what this server is configured to hold.

### Response shapes

A successful single value, one entry of a batch response, or one SSE update frame all share one JSON shape:

```json
{
  "name": "db-password",
  "bytes": "aHVudGVyMg==",
  "version": "v3",
  "sensitive": true,
  "not_after": "2026-08-01T00:00:00Z",
  "metadata": {},
  "kind": ""
}
```

- **`bytes`** is base64-encoded - this is just `encoding/json`'s standard `[]byte` handling, not a custom encoding.
- **`version`**, **`sensitive`**, **`not_after`**, and **`kind`** are all `omitempty`; `metadata` is never omitted (a `nil` metadata map from the provider is normalized to `{}` on the wire, never JSON `null`).
- **`kind`** is empty for a fresh value. See [The `kind` field: fresh vs. stale-but-serving](#the-kind-field-fresh-vs-stale-but-serving) below for what a non-empty `kind` on a *successful* response means.

A whole-request failure (auth failure, a malformed batch body, `GET /v1/values/{name}` for an unbound or unresolvable name) is:

```json
{"error": {"kind": "not_found", "message": "binding not found"}}
```

A batch or watch response never fails the whole request over one bad name: a single name that is denied, unbound, or unresolvable becomes exactly that one entry in the response, shaped the same as an error but carrying the requested `name`:

```json
{"name": "some-other-binding", "error": {"kind": "permission_denied", "message": "permission denied"}}
```

`kind` round-trips through core's `mamori.ErrorKind`/`mamori.SentinelFor`, so a client can recover the original sentinel error: `mamori.SentinelFor(mamori.Kind("permission_denied"))` gives back `mamori.ErrPermissionDenied`. The kind-to-HTTP-status mapping:

| `kind` | HTTP status |
| --- | --- |
| `not_found` | 404 |
| `invalid` | 400 |
| `unauthenticated` | 401 |
| `permission_denied` | 403 |
| `rate_limited` | 429 |
| `unavailable` | 503 |
| `unknown` | 500 |

### The `kind` field: fresh vs. stale-but-serving

`kind` appears in two structurally different places, and conflating them is the single easiest mistake a client author can make:

- **On an error entry** (`{"error":{"kind":...}}`), `kind` describes why the request failed. This is a failure signal.
- **On a successful value** (`bytes` present, no `error` field), a non-empty `kind` means "this is a last-known-good value being served while the binding's upstream is *currently* failing" - stale-but-serving, not a fresh resolve. **This is an annotation on a success, never a failure by itself.** A client that only checks "is `bytes` present" and ignores `kind` will silently keep using a value the server itself knows is stale; a client that treats any non-empty `kind` as an error will incorrectly reject perfectly usable data. Check for the presence of `error` to know success vs. failure, and use `kind` on a success only to decide whether to log or alert on staleness.

### The zero-length-value wire wrinkle

`bytes`, `version`, `sensitive`, and `not_after` are all `omitempty`. This has one deliberate, documented consequence: a genuinely empty (zero-length) secret has its `bytes` key omitted from the JSON exactly the same way an error entry never has a `bytes` key at all. **Success and failure are distinguished by the presence of the `error` field, never by whether `bytes` is present or empty.** A client that infers "no bytes means error" will misread a legitimate zero-length value as a failure.

### `GET /v1/watch` is a 200ms poll, not push

```text
GET /v1/watch?name=db-password&name=api-key
```

`/v1/watch` opens a Server-Sent Events stream: an `error` frame at subscribe time for any denied or unbound name (the stream still opens and stays live for whatever names *were* allowed), then an `update` frame each time an allowed name's resolved state changes, plus a periodic heartbeat comment (an SSE comment line, ignored by clients) so idle-closing proxies do not drop the connection.

Be precise about what "changes" means here: **the resolver exposes no push notification.** `handleWatch` polls each subscribed binding's local, already-resolved snapshot every 200ms and sends a frame only when that snapshot actually differs from what it last sent. This is not instant push from the upstream backend - it is a fast poll of state that is itself already kept current by each binding's own `WatchRef` goroutine. In practice this means a change lands on an open `/v1/watch` connection within roughly 200ms of the binding's own snapshot updating, which is usually indistinguishable from push, but a client should not assume sub-poll-interval latency or build logic that depends on it.

The stream ends when the client disconnects or a write fails; reconnecting afterward is the client's responsibility.

### `GET /v1/healthz`: bare liveness, nothing else

```json
{"status": "ok"}
```

Fixed, unconditional `200`, never anything else, never any binding detail - it is the one route exempt from authentication and authorization specifically so that a liveness/readiness probe with no credential at all still works, and specifically shaped to never become a way to learn what this server is configured to serve (no binding names, no per-binding health, nothing that varies with what is bound).

## Audit: never the value

```go
func WithAudit(l *slog.Logger) Option
```

Audit logging is off by default (`nil` logger) - it is a diagnostic aid, not the enforcement mechanism; `Policy` and `Authenticator` are what actually decide access. When configured, one structured record is written per request (per name, for a batch or watch subscription), carrying identity subject, binding name, allow/deny decision, resulting `kind`, HTTP status, and latency.

**The audit record structurally cannot carry a resolved value.** This is not "the logging code is careful not to log the value" - it is that the Go type backing an audit record (`requestOutcome`) has no field a value's bytes could be assigned to. There is no line of code that could accidentally wire a `mamori.Value` into an audit log, because there is nowhere on the struct to put it.

## Blast radius

State this plainly, because it is the single most important fact about this component: **a config server is deliberately the highest-blast-radius piece of mamori.** A single `Load` or `Watch` caller holds only the credentials it was configured with. A config server, by design, concentrates *every* backend credential its bindings touch into one process, reachable by every consumer it serves. Compromising this process is strictly worse than compromising any one of its consumers - an attacker who gains code execution here potentially gains everything every binding can reach, not one workload's slice of it.

The mitigations in this module are structural, not conventions an operator has to remember to follow correctly:

- **No client-supplied refs.** A client can only ever name a binding by the name the operator chose; it can never supply a ref. This alone eliminates the file-read/exec-injection class of vulnerability (`file:///etc/shadow`, an arbitrary `exec:` command) that a naive "resolve whatever ref the client asks for" design would open.
- **Mandatory `Policy`.** `New` will not construct a `Server` with no authorization policy - not even a deny-all default. Every deployment must make, and see itself make, an explicit choice.
- **Mandatory authentication over the network.** `NoAuth()` is refused outright when a TCP listener is configured; the only unauthenticated posture this module allows is a Unix-socket-only deployment where the OS itself is the access boundary.
- **Mandatory TLS over TCP.** Plaintext TCP requires the deliberately uncomfortable, greppable `InsecureNoTLS()` opt-out; the default is TLS or refuse to construct.
- **No values in the audit trail.** An audit log that captured resolved secrets would itself become a second, lower-scrutiny copy of every credential this server holds - structurally impossible here, not merely discouraged.

None of this changes the underlying fact: the credentials are still all in one place. These mitigations narrow *how* that concentration can be abused; they do not undo the concentration itself. Weigh that against the [fan-out benefits](#why-fan-out-through-a-server-at-all) above for your own deployment - a trusted sidecar process reading a handful of bindings over a `0600` Unix socket is a very different risk than a network-reachable server fronting every credential a large fleet needs, even though both are the same module.

Treat the config server's own host and process the way you would treat a secrets-manager credential itself: minimal blast radius around *it* (least-privilege IAM for the backends it talks to, no unrelated workloads on the same host, its logs and audit trail held to the same bar as the secrets it serves), because everything downstream of it inherits whatever happens to it.

## See also

[Auth](../auth) covers every shipped `Authenticator`, including `PeerCred` in full. [Security & releases](../security) covers the two HTTP surfaces mamori exposes side by side (the metadata-only admin endpoint and this server) and the blast-radius point again in that context. [Observability](../observability) covers `WithAdminHTTP`, the metadata-only sibling to this server that answers "is my config healthy" rather than "give me a value."
