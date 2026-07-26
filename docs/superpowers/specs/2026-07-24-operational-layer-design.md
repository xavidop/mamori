# mamori operational layer: status, doctor, test kit, history, source chains, CLI

**Date:** 2026-07-24
**Status:** approved, not yet implemented
**Scope:** core module, `providertest`, all 31 provider modules, `x/otel`, new `cmd/mamori` module, README and docs site

---

## 1. Context

mamori v1.1.7 has broad provider coverage (31 modules), middleware, an OTel bridge,
a `go vet` analyzer, and a conformance kit. What it lacks is the operational layer
around the reconciliation loop it advertises.

Concretely, `Watcher[T]` exposes exactly two methods, `Get` and `Close`
(`reconciler.go:56-65`). The engine already tracks per-field state internally
(`observed`, `applied`, `lastOK` at `reconciler.go:132-136`) but none of it is
reachable. An operator cannot answer "which field is stale, when did it last
refresh, which provider is failing". `WithStale` and `OnError` push information
out; nothing lets you pull it.

A second gap compounds the first: failures are not classifiable. `errors.go`
defines a single `ErrNotFound` sentinel, so "the secret does not exist" and
"this IAM role may not read it" both surface as an opaque `ProviderError`. Any
status or preflight feature built on top of that reports "field failed", which is
not actionable.

This spec covers five additions that close that layer.

## 2. Goals

1. Make the running reconciler introspectable: per-field status, health, and an
   HTTP endpoint.
2. Make config failures detectable before deploy, using the application's own
   provider wiring.
3. Let application authors test their `OnChange` handlers deterministically.
4. Let operators freeze and inspect config transitions in production.
5. Let a field draw from a precedence chain of sources rather than exactly one.
6. Let one process front the backends for many, so credentials concentrate in one
   place and N consumers cost one upstream watch rather than N.

## 3. Non-goals

The original design stated "not a secrets store, not a sync engine". Workstreams
H and I move that line deliberately, so it is restated precisely rather than
left to erode:

- mamori **is not a secrets store**. The server holds no persistent state, is
  never the system of record, and owns no durable copy of any value. It is a
  read-through fan-out of upstream providers: kill it and restart it and nothing
  is lost. Every value it serves has an upstream owner.
- mamori **does not write**. Nothing in this spec mutates a backend, including
  the server.
- The server **does not resolve client-supplied refs** (decision D9). It serves a
  fixed set of operator-declared bindings. This is what keeps it a fan-out rather
  than a general-purpose remote-resolution service.

Still out of scope, unchanged:

- No feature-flag system, no cross-language support.
- The CLI never resolves refs itself (decision D1). `mamori doctor` is a client of
  a running process's admin API, so it links no provider modules.
- No automatic rollback. Pinning is an explicit operator action.
- No OpenFeature bridge. Considered and deferred; orthogonal to this layer.
- No server-side persistence, replication, leader election, or clustering. Run
  more instances; they share no state by construction.

## 4. Decisions

| ID | Decision | Rationale |
|----|----------|-----------|
| D1 | `Doctor` is a **library function** in core; the CLI **never resolves**, it either reads source or reads a running process's admin API | A CLI that resolved would have to link all 31 provider modules and still could not see custom providers, middleware chains, or `Prefix` rewriting. A library call runs the user's exact wiring. `mamori doctor` against a live admin endpoint gets the same fidelity for free, because the process being inspected already did the resolving, so the CLI stays dependency-light either way. |
| D2 | Source chains mean **precedence / layering**, not failover | Availability already has `middleware.Failover`. Overloading the chain with two meanings makes watch semantics ambiguous. Each mechanism means exactly one thing. |
| D3 | History gives **Pin/Unpin**, not one-shot rollback | The engine already rejects invalid snapshots, so last-good is always current. Rollback only matters for a *valid but wrong* config, and a one-shot revert is undone by the next reconcile. Pinning answers "who wins next" unambiguously. |
| D4 | **Full error-classification sweep**, conformance-enforced | A partially classified doctor is worse than none, because `Unknown` becomes indistinguishable from "provider not updated yet". |
| D5 | The admin endpoint serves **metadata only, with no option to serve values** | A structural boundary beats a default: a config-wide exposure toggle is the kind of thing flipped during an incident and never flipped back. It would also have misled, since `json.Marshal` of `T` redacts every `secret.String` and so would have exposed only the non-secret fields while risking all of them. Real values are the config server's job (workstream H), where policy and audit govern every read. |
| D6 | Snapshot history defaults to **off** | Retained snapshots hold full `T` values, including rotated secret material. Defaulting to 10 would keep ten generations of every secret in memory in a library whose headline feature is secret hygiene. |
| D7 | mamori can run its **own admin HTTP server**, off by default | Services without an existing mux should not have to build one to get a readiness probe. Off means inert: no listener, no bound port, no goroutine. The `Handler` constructor remains for services that already have a mux. |
| D8 | Auth is one `Authenticator` interface; stdlib schemes in core, JWT in `x/authjwt`; `/healthz` is **exempt but detail-free** | When mamori owns the server there is nowhere for the caller to wrap middleware, so an extension point is mandatory. One interface supports every scheme without shipping every scheme, mirroring the provider ecosystem. Probes work unconfigured, while unauthenticated callers learn liveness but not field paths, refs, or error kinds. |
| D9 | The config server serves **operator-declared named bindings**, never client-supplied refs | A server that resolves arbitrary client refs is a remote file-read and command-execution primitive: `file:///etc/shadow` and `exec:` would be reachable from any authenticated client. Bindings delete the entire injection class, and they decouple clients from backends, so moving a secret from AWS to Vault is a server-side edit with no client change. |
| D10 | The server is a **library plus your own binary**, not a CLI subcommand | Serving requires provider modules linked in, which would drag every cloud SDK into the `mamori` CLI and break decision D1. As a library, your sidecar binary contains only the providers you actually import, which is the same argument that makes providers separate modules today. |
| D11 | The server listens on **TCP and Unix domain sockets**, same protocol | A UDS is the correct transport for the sidecar shape: no network exposure, filesystem permissions as coarse authorization, and kernel-verified peer credentials as strong authentication. It is the same `http.Handler` on a different `net.Listener`, so the protocol does not fork. |
| D12 | TLS is **mandatory on TCP**, exempt on UDS | The server transmits real secret values, unlike the admin endpoint. A UDS never leaves the host and is protected by file permissions, so requiring certificates there is friction with no benefit. On TCP, refusing to start without TLS is the only default that is not a footgun. |

## 5. Architecture

Nine workstreams. A is the foundation for everything. B, C, and D consume its
report type. E changes core resolution semantics and lands after B can observe
it. H and I depend on A for wire-level error classification and on B's
`Authenticator` for access control; I depends on H's protocol. F and G are
downstream.

```
A. Error kinds ──┬─→ B. Status / Health / Handler / Doctor ──┬─→ F. CLI
                 │        └─→ Authenticator ──┐               │
                 ├─→ C. mamoritest            │               │
                 │                            ▼               │
                 ├─→ E. Source chains    H. Config server      │
                 │                            │               │
                 │                            ▼               │
                 │                       I. mamori:// client ──┤
                 │                                            │
                 └─→ D. History + Pin ───────────────────────→ G. Docs sweep
```

### 5.1 New public surface

Core module (`github.com/xavidop/mamori`), no new dependencies:

| File | Additions |
|------|-----------|
| `errors.go` | 5 new sentinels, `Kind` type, `ErrorKind(error) Kind`, `HealthError`, `ErrNoSuchSnapshot` |
| `status.go` (new) | `FieldStatus`, `Report`, report construction |
| `doctor.go` (new) | `Doctor[T](ctx, ...Option) (Report, error)` |
| `handler.go` (new) | `Handler[T]`, `HandlerOption`, `HandlerPrefix`, `WithAdminHTTP`, `WithAdminTLS` |
| `handlerauth.go` (new) | `HandlerMiddleware`, `BasicAuth`, `BasicAuthFunc`, `BearerToken`, `BearerTokenFunc` |
| `history.go` (new) | `Snapshot[T]`, `WithHistory` |
| `reconciler.go` | `Watcher.Status`, `.Health`, `.History`, `.Pin`, `.PinCurrent`, `.Unpin`, `.Pinned` |
| `watchref.go` (new) | `WatchRef`, extracted from `engine.start`; needed by the server |
| `ref.go` | `ParseRefs(tag) ([]Ref, error)` |
| `decode.go` | `fieldSpec.Ref` → `fieldSpec.Refs []Ref`, `fieldSpec.OnFail` |
| `resolve.go`, `poll.go` | chain-aware resolution and watching |
| `mamoritest/` (new pkg) | consumer test kit |

Other modules:

| Module | Change |
|--------|--------|
| `providertest` | `Config.Fail` hook (required), new `ErrorClassification` conformance case |
| 31 `providers/*` | SDK error mapping + per-provider mapping table test + `Fail` hook in conformance wiring |
| `x/otel` | record `mamori.error.kind` on the resolve span and the `mamori.resolve.duration` histogram. Not on `mamori.watch.errors`: `Meter.RecordWatchError(scheme string)` (`observ.go:12`) takes no error, so carrying a kind there would mean a breaking change to the `Meter` interface for marginal value. Deferred deliberately. |
| `x/authjwt` (new module) | JWT validation with JWKS fetch and cache |
| `server` (new module) | config server; depends on core plus stdlib only |
| `providers/mamori` (new module) | `mamori://` client; native watch over SSE |
| `cmd/mamori` (new module) | `explain`, `schema`, `policy`; depends on `golang.org/x/tools/go/packages` |

One new **core** dependency, called out because the rest of this spec adds none:
`golang.org/x/sys`, promoted from indirect to direct, required by `PeerCred` for
`SO_PEERCRED` on Linux and `LOCAL_PEERCRED` on Darwin. The stdlib `syscall`
package covers Linux alone; hand-rolling the Darwin path with raw `getsockopt`
for a security primitive is the worse trade. It is a `golang.org/x` module
already present in the module graph, and it does not weaken the actual promise,
which is that core pulls in no cloud SDK.

---

## 6. Workstream A: error classification

### 6.1 Design

```go
// Kind is a coarse, provider-independent classification of a resolve failure.
type Kind string

const (
	KindNotFound         Kind = "not_found"
	KindPermissionDenied Kind = "permission_denied"
	KindUnauthenticated  Kind = "unauthenticated"
	KindUnavailable      Kind = "unavailable"
	KindRateLimited      Kind = "rate_limited"
	KindInvalid          Kind = "invalid"
	KindUnknown          Kind = "unknown"
)

var (
	ErrNotFound         = errors.New("mamori: not found")          // unchanged
	ErrPermissionDenied = errors.New("mamori: permission denied")
	ErrUnauthenticated  = errors.New("mamori: unauthenticated")
	ErrUnavailable      = errors.New("mamori: unavailable")
	ErrRateLimited      = errors.New("mamori: rate limited")
	ErrInvalid          = errors.New("mamori: invalid reference")
)

// ErrorKind classifies err by walking the errors.Is chain. Unrecognized errors
// return KindUnknown, which is a legal and honest answer, not a failure.
func ErrorKind(err error) Kind
```

Semantics preserved: `ErrNotFound` remains the **only** kind that triggers
default application and `optional` handling. The other kinds are diagnostic;
they do not change resolution behavior except where `onfail` (workstream E)
explicitly acts on them.

Providers classify by wrapping:

```go
// providers/aws/sm.go
var ae smithy.APIError
if errors.As(err, &ae) {
    switch ae.ErrorCode() {
    case "ResourceNotFoundException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrNotFound, err)
    case "AccessDeniedException", "UnrecognizedClientException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)
    case "ThrottlingException":
        return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrRateLimited, err)
    }
}
```

`ProviderError` is unchanged. Its existing `Unwrap` (`errors.go:28`) already lets
`ErrorKind` reach the wrapped sentinel, so no change is needed there.

### 6.2 Enforcement

`providertest` runs against in-memory fakes, which cannot produce a genuine IAM
denial. Enforcement therefore splits in two, and the spec is explicit that
neither half alone is sufficient:

**Half 1, mechanically enforced for every provider.** `providertest.Config` gains:

```go
// Fail makes the backend return err for key on the next resolve and on any
// active watch. Required: the ErrorClassification conformance case cannot run
// without it.
Fail func(ctx context.Context, key string, err error) error
```

The new `ErrorClassification` case injects each sentinel through `Fail` and
asserts the kind survives both `Resolve` and `Watch` unchanged. This catches the
common real bug: a provider that catches an error, reformats it with
`fmt.Errorf("...: %v", err)`, and destroys the `errors.Is` chain.

`providertest.Run` ends up failing with an explicit message when `Fail` is nil,
but it gets there in two steps so the tree is never red. The hook lands first
with the case skipping when it is absent; the sweep adds it to all 31 modules;
the final task of the sweep flips it to required. Flipping it in one step would
break every provider's conformance test the moment the kit changed.

The end state is a **breaking change to the conformance kit**. External
providers pin their own core version and upgrade on their own schedule; the
CHANGELOG and `writing-a-provider.md` must call this out.

**Half 2, per provider.** Each provider module gains `<name>_errors_test.go`: a
table mapping real SDK error values to expected kinds. These are the only tests
that verify the actual mapping, and they run without a live backend because SDK
error types are constructible.

### 6.3 Kind mapping guidance

Provider authors follow this table. Documented in `writing-a-provider.md`.

| Kind | Use for |
|------|---------|
| `KindNotFound` | Key, secret, path, or version genuinely absent |
| `KindPermissionDenied` | Authenticated but not authorized (IAM deny, Vault policy, RBAC) |
| `KindUnauthenticated` | Missing, malformed, or expired credentials; failed token renewal |
| `KindUnavailable` | Network failure, DNS, timeout, 5xx, circuit open |
| `KindRateLimited` | Throttling, quota exhaustion, 429 |
| `KindInvalid` | Ref is malformed for this provider, or the payload cannot be parsed |
| `KindUnknown` | Anything else. Explicitly acceptable. |

---

## 7. Workstream B: status, health, doctor, HTTP exposure, auth

### 7.1 Report types

```go
type FieldStatus struct {
	Path       string        // dotted field path, e.g. "Redis.Password"
	Scheme     string        // scheme of the winning ref
	Ref        string        // winning ref, with sensitive opts redacted
	Candidates []string      // full chain, redacted, in precedence order
	Version    string        // provider version of the currently observed value
	LastOK     time.Time     // last successful resolve
	Age        time.Duration // GeneratedAt - LastOK
	Stale      bool          // Age > WithStale, when WithStale is configured
	LastError  string        // last error text, "" if none
	LastKind   Kind          // classification of LastError, "" if none
	Sensitive  bool          // field is secret.String / secret.Bytes
}

type Report struct {
	Fields      []FieldStatus // in struct declaration order
	Snapshot    uint64        // version of the snapshot Get() currently returns
	Live        uint64        // latest validated snapshot; differs from Snapshot only while pinned
	Pinned      bool
	Healthy     bool
	GeneratedAt time.Time
}
```

`Ref` and `Candidates` are redacted: any query option whose name matches a
denylist (`token`, `password`, `secret`, `key`, `apikey`, `api_key`, `sas`,
`credential`) has its value replaced with the `secret.Redacted` constant. Refs
are not generally secret, but some providers accept inline credentials as opts
and the report is designed to be safe to serve over HTTP.

### 7.2 Concurrency

The engine's per-field maps are owned by the reconciler goroutine and carry no
locks. Rather than introduce a mutex on the hot path, the engine builds an
immutable `*Report` and publishes it through an `atomic.Pointer[Report]` at the
end of each `loop` iteration and at the end of `flush`. Readers get a lock-free,
internally consistent snapshot, matching how `Get` already works
(`reconciler.go:56`).

`GeneratedAt` and `Age` are therefore as of the last engine iteration, not as of
the `Status()` call. `Status` recomputes `Age` and `Stale` against
`clock.Now()` at read time so an idle engine does not report stale ages as fresh.

### 7.3 API

```go
func (w *Watcher[T]) Status() Report

// Health returns nil when every field is fresh and no field's last error is a
// terminal kind. Terminal kinds are NotFound, PermissionDenied, Unauthenticated,
// and Invalid: conditions that will not resolve without human action. Unavailable
// and RateLimited are transient and do not fail Health unless the field has also
// exceeded WithStale. Intended for a Kubernetes readiness probe.
func (w *Watcher[T]) Health() error
```

`Health` returns a `HealthError` wrapping the offending `FieldStatus` entries, so
callers can log specifics rather than a bare "unhealthy".

### 7.4 Doctor

```go
// Doctor resolves every field of T exactly once and returns a Report describing
// what succeeded and what failed, without starting a watcher. It accepts the same
// Options as Load and Watch, so it exercises the caller's real provider wiring,
// middleware, and Prefix rewriting.
//
// The returned error is non-nil only when the config type itself cannot be
// walked. Individual field failures are reported in the Report, not returned,
// so a caller sees every problem at once rather than the first.
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error)
```

`Doctor` reuses `fieldSpecs` and `resolveAll` and does not validate or decode; a
field that resolves but fails validation is out of scope for a reachability
check and is already covered by `Load`.

`Report.Healthy` is false if any field failed, with one exception: a
`KindNotFound` on a field that carries a `default:` tag or is marked `optional`
is an expected, healthy outcome, since that field resolves successfully in
practice. Every other non-empty `LastKind` makes the report unhealthy.

There are two ways to reach a `Report`, and they are complementary rather than
redundant. `mamori.Doctor` runs **before** the process starts, in CI, and answers
"would this deploy resolve". `mamori doctor` (section 11.2) queries a **running**
process's admin endpoint and answers "is the thing in production healthy right
now". Both render the identical `Report` type, so the output is the same shape in
CI and in an incident.

Documented CI pattern, for the pre-deploy half (in `docs/testing.md`):

```go
//go:build preflight

func TestConfigPreflight(t *testing.T) {
	rep, err := mamori.Doctor[Config](context.Background(), appProviders()...)
	if err != nil { t.Fatal(err) }
	for _, f := range rep.Fields {
		if f.LastKind != "" {
			t.Errorf("%s (%s): %s: %s", f.Path, f.Ref, f.LastKind, f.LastError)
		}
	}
}
```

### 7.5 HTTP exposure

Two independent ways to expose the report over HTTP. Both are off by default,
and "off" means genuinely inert: no listener, no bound port, no goroutine.

**Mount it yourself.** `Handler` returns an `http.Handler` for an existing mux.
Nothing runs until the caller mounts it.

```go
type HandlerOption func(*handlerOptions)

// HandlerPrefix strips prefix from request paths before routing.
func HandlerPrefix(prefix string) HandlerOption

func Handler[T any](w *Watcher[T], opts ...HandlerOption) http.Handler
```

**The admin endpoint never serves configuration values.** It has no route that
can, under any option. Metadata only: field paths, redacted refs, versions, ages,
staleness, and error kinds.

This is a hard structural boundary rather than a default, because a toggle that
exposes an entire config over HTTP is exactly the kind of thing that gets flipped
during an incident and never flipped back. It also removes a subtlety that would
have misled people: `json.Marshal` of `T` renders every `secret.String` as
`[REDACTED]` (`secret/secret.go:44`), so such a route would have served useful
data only for the non-secret fields while carrying the risk of all of them.

Serving real values is the config server's job (workstream H), where bindings,
an explicit authorization policy, mandatory TLS on TCP, and an audit log govern
every read. The two surfaces stay on separate ports and separate code paths, so
no configuration mistake on one can turn it into the other.

**Let mamori serve it.** For services with no mux of their own:

```go
// WithAdminHTTP makes the Watcher run its own HTTP server on addr, serving the
// same routes as Handler. It is off by default: with no WithAdminHTTP option no
// server is constructed, no port is bound, and no goroutine is started. The
// server's lifetime is the Watcher's; Close shuts it down gracefully.
func WithAdminHTTP(addr string, opts ...HandlerOption) Option
```

Lifecycle, and the reason it is an `Option` on `Watch` rather than a free
function: the server must not outlive the watcher it reports on. `Watch` binds
the listener before returning, so a bind failure (port in use, no permission)
fails `Watch` with the bind error rather than logging it and leaving the caller
believing the endpoint is up. This matches `Watch`'s existing fail-fast contract
around the initial `Load` (`reconciler.go:83-86`). `Close` calls
`Shutdown` with a bounded grace period and waits for it, so `Close` returning
means the port is released.

`Load` takes the same `Option` type but has no watcher to report on, so
`WithAdminHTTP` is ignored there and documented as such.

Routes, identical in both modes:

| Path | Response |
|------|----------|
| `GET /` | `Report` as JSON |
| `GET /healthz` | 200 with `{"status":"ok"}`, or 503; failing-field detail only when authenticated (see 7.6) |

That is the complete route set. There is no third route and no option that adds
one.

The surface is read-only in both modes. `Pin` and `Unpin` are deliberately not
exposed over HTTP: they change application behavior, and a mistaken pin is a
silent production incident. An application that wants remote pinning should mount
its own route calling `w.Pin` behind its existing authorization.

`net/http` is stdlib, so the core dependency policy (stdlib plus `validator`,
`mapstructure`, `fsnotify`, and `yaml.v3`) is unaffected.

### 7.6 Authentication and TLS

When mamori owns the server the caller has nowhere to wrap middleware, so this
is part of the design rather than an afterthought. Auth follows the same shape
as the provider ecosystem: one small interface in core, stdlib-only
implementations in core, heavier schemes in their own modules.

```go
// Authenticator decides whether a request may proceed, and says who the caller
// is. A nil error allows the request; any error denies it.
//
// The returned Identity is ignored by the admin endpoint and required by the
// config server, whose authorization policy is expressed in terms of it. It is
// one interface rather than two so an Authenticator written for one surface
// works unchanged on the other.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Identity is the authenticated caller. Subject is a stable principal name;
// Attrs carries scheme-specific detail (JWT claims, certificate SANs, peer uid).
type Identity struct {
	Subject string
	Attrs   map[string]string
}

// Challenger is optionally implemented by an Authenticator to supply the
// WWW-Authenticate header value sent with a 401.
type Challenger interface {
	Challenge() string
}

// AuthFunc adapts a plain function to Authenticator.
type AuthFunc func(*http.Request) (Identity, error)

// ErrForbidden, returned from Authenticate, produces 403 rather than 401. Use it
// when the caller is authenticated but not permitted.
var ErrForbidden = errors.New("mamori: forbidden")

func WithAuth(a Authenticator) HandlerOption

// HandlerMiddleware remains for concerns that are not authentication: request
// logging, rate limiting, CORS. It runs outside the Authenticator.
func HandlerMiddleware(mw func(http.Handler) http.Handler) HandlerOption

// WithAdminTLS serves the admin endpoint over TLS, and carries the ClientCAs and
// ClientAuth settings that MTLS depends on. Basic auth over plaintext is not
// authentication; this exists so the shipped schemes can be used safely.
func WithAdminTLS(cfg *tls.Config) Option
```

One interface means mamori supports every scheme without shipping every scheme.
JWT, OIDC, HMAC signatures, IAP headers, Kubernetes `TokenReview`, or a service
mesh identity header are all "inspect the request, say yes or no".

**Shipped in core**, stdlib only, so the dependency policy is unaffected:

```go
func BasicAuth(user string, pass secret.String) Authenticator
func BasicAuthFunc(fn func() (string, secret.String)) Authenticator
func BearerToken(tok secret.String) Authenticator
func BearerTokenFunc(fn func() secret.String) Authenticator
func APIKey(header string, key secret.String) Authenticator
func APIKeyFunc(header string, fn func() secret.String) Authenticator
func MTLS(opts MTLSOptions) Authenticator
func PeerCred(opts PeerCredOptions) Authenticator
func AnyOf(as ...Authenticator) Authenticator   // first success wins
func AllOf(as ...Authenticator) Authenticator   // all must pass
```

`PeerCred` authenticates a Unix-domain-socket client by the kernel-verified uid
and gid of the connecting process, which the caller cannot forge. It is the
strongest and simplest option for a sidecar, and it sets
`Identity{Subject: "uid:1001", Attrs: {"uid","gid","pid"}}`.

Portability is the catch and must be handled explicitly rather than discovered
later: it is `SO_PEERCRED` on Linux and `LOCAL_PEERCRED` with `xucred` on
Darwin/BSD, so the implementation is build-tagged per platform. Windows supports
`AF_UNIX` but exposes no peer credentials, so the Windows build returns an
authenticator that **denies every request** with a clear error rather than
silently allowing. `PeerCred` on a TCP connection is likewise a hard denial, not
a fallthrough.

`MTLS` verifies `r.TLS.PeerCertificates` against allowed subject common names or
DNS SANs, and requires `WithAdminTLS` with `ClientAuth: tls.RequireAndVerifyClientCert`.
Constructing `MTLS` without TLS configured is a construction error rather than a
silently-open endpoint. For an endpoint reporting on secret material inside a
cluster, this is the strongest option available without a dependency, and the
docs should present it as the recommended one.

**Shipped as `x/authjwt`**, a new module alongside `x/otel`:

```go
mamori.WithAuth(authjwt.New(authjwt.Config{
	JWKSURL:  "https://idp.example.com/.well-known/jwks.json",
	Issuer:   "https://idp.example.com/",
	Audience: "mamori-admin",
}))
```

JWKS keys are fetched once and cached with a refresh on unknown `kid`, bounded by
a minimum refresh interval so an attacker cannot drive unbounded fetches by
presenting random `kid` values. Algorithms are allowlisted explicitly and `none`
is rejected, which closes the classic algorithm-confusion hole. OIDC discovery
and token introspection are deliberately out of scope: rare for an admin port,
and a large dependency tree to own.

Implementation requirements for the core schemes, all testable:

- Credential comparison is constant-time via `crypto/subtle.ConstantTimeCompare`,
  including the username component of basic auth, so the endpoint discloses
  neither the secret through response timing nor whether a username exists.
- Credentials are `secret.String`, so they redact in logs like every other secret
  in mamori.
- A zero `secret.String` from any `Func` variant denies every request. It never
  falls through to an open endpoint and never panics.
- `BasicAuth` challenges with `Basic realm="mamori"`; `BearerToken` with `Bearer`.
  `APIKey` and `MTLS` implement no `Challenger` and send a bare 401.
- Authentication failures are never logged with the presented credential.
- `AnyOf` evaluates every member even after one fails, so its total work does not
  depend on which member rejected. It returns the first member's `Challenge`.
- Passing `WithAuth` more than once is a construction error, not last-wins.
  Compose with `AnyOf` or `AllOf` instead.

**Probe exemption (decision D8).** `/healthz` answers without credentials, but
the unauthenticated response carries only the bare status. The failing-field
detail, which includes field paths, redacted refs, and error kinds, is served
only to an authenticated caller. So a Kubernetes probe needs no configuration
while a port-scanner learns nothing about the config surface.

```
GET /healthz              (no credentials)
  200 {"status":"ok"}
  503 {"status":"unhealthy"}

GET /healthz              (authenticated)
  503 {"status":"unhealthy","fields":[
        {"path":"DBPassword","ref":"aws-sm://prod/db#password",
         "kind":"permission_denied"}]}

GET /                     (no credentials)
  401
```

With auth not configured, `/healthz` returns the full detail as before: there is
no credential to distinguish callers by, and the operator has accepted that
posture by binding the port.

**Fail closed on an unset credential.** A zero `secret.String` returned from
either `Func` variant denies every request with 401. It never falls through to
an open endpoint, and it never panics. This makes the rotation pattern below
safe during the window before the credential is first populated, and it makes a
misconfigured `Func` fail in the safe direction.

Rotating the admin credential through mamori itself:

```go
// Seed from a one-shot Load so the credential exists before the listener binds.
boot, err := mamori.Load[Config](ctx)
if err != nil {
    return err
}
var admin atomic.Pointer[secret.String]
tok := boot.AdminToken
admin.Store(&tok)

w, err := mamori.Watch[Config](ctx,
    mamori.OnChange(func(ev mamori.Change[Config]) {
        if ev.Changed("AdminToken") {
            next := ev.New.AdminToken
            admin.Store(&next)
        }
    }),
    mamori.WithAdminHTTP(":9090",
        mamori.BearerTokenFunc(func() secret.String {
            if p := admin.Load(); p != nil {
                return *p
            }
            return secret.String{} // fails closed
        })),
    mamori.WithAdminTLS(tlsCfg),
)
```

The indirection through an `atomic.Pointer` rather than a closure over `w` is
forced, not stylistic: options are evaluated inside `Watch`, before `w` exists,
so a closure over `w` captures a nil pointer and panics on the first request.
The docs must show the seeded form above, because the naive version compiles,
passes a smoke test that never hits the endpoint, and fails in production.

---

## 8. Workstream C: mamoritest

`providertest` serves provider authors. Application authors currently have no way
to test that their `OnChange` handler rotates a pool, because doing so requires a
real backend and a real clock.

```go
package mamoritest

// Provider is an in-memory, scriptable mamori.Provider for tests. It implements
// WatchableProvider, so changes are delivered natively rather than by polling.
type Provider struct{ ... }

func NewProvider(scheme string) *Provider

func (p *Provider) Set(key, val string)          // seed or update; pushes to watchers
func (p *Provider) SetBytes(key string, b []byte)
func (p *Provider) Del(key string)               // subsequent resolves return ErrNotFound
func (p *Provider) Fail(key string, err error)   // resolves and watches return err
func (p *Provider) Clear(key string)             // cancel a Fail

// WaitForSnapshot blocks until the watcher has applied snapshot version v, or
// fails the test after a bounded timeout. Deterministic: no sleeps.
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64)

// WaitForError blocks until OnError has been called with an error of the given
// kind, or fails the test. Requires CaptureErrors to be installed.
func WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error

// CaptureErrors returns an Option installing an OnError sink plus its capture.
func CaptureErrors() (mamori.Option, *ErrorCapture)
```

Usage:

```go
p := mamoritest.NewProvider("test")
p.Set("db/password", "hunter2")
onErr, errs := mamoritest.CaptureErrors()

w, err := mamori.Watch[Config](ctx, mamori.WithProvider(p), onErr)
// ...
p.Set("db/password", "rotated")
mamoritest.WaitForSnapshot(t, w, 2)
if got := pool.Current(); got != "rotated" { t.Fatal(...) }

p.Fail("db/password", mamori.ErrPermissionDenied)
mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
```

Placement: a subpackage of the core module. It imports `testing` for `testing.TB`,
which is the same approach `net/http/httptest` takes; no new module dependency.

`WaitForSnapshot` polls `w.Status().Snapshot` with a backoff bounded by a
deadline rather than sleeping a fixed duration, which is why workstream B lands
first. The existing `NewFakeClock` (`clock.go:91`) remains available and is
documented for tests that need to drive poll intervals, but the test kit itself
does not require it.

---

## 9. Workstream D: history and pin

```go
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot
}

// WithHistory retains the n most recent snapshots in addition to the current one.
// It defaults to 0.
//
// Retained snapshots hold full copies of T, including any secret material that
// has since been rotated. Enabling history extends the in-memory lifetime of old
// secrets. Enable it deliberately.
func WithHistory(n int) Option

func (w *Watcher[T]) History() []Snapshot[T]      // newest first; current snapshot always included
func (w *Watcher[T]) Pin(version uint64) error    // ErrNoSuchSnapshot if not retained
func (w *Watcher[T]) PinCurrent() uint64          // freeze at whatever Get() returns now
func (w *Watcher[T]) Unpin()
func (w *Watcher[T]) Pinned() (uint64, bool)
```

### 9.1 Versioning

Snapshot versions are monotonic `uint64` starting at 1, assigned to the initial
snapshot produced by `Watch`'s fail-fast `Load` (`reconciler.go:83`). Each
successful `flush` that produces a non-empty `Fields` diff increments it.

### 9.2 Pin semantics

While pinned, `flush` continues to build, decode, and validate candidate
snapshots exactly as before, and continues to record them as `Live`. It does not
call `cfg.Store` and does not enqueue a `Change`. Consequently:

- `Get()` keeps returning the pinned snapshot.
- Watches stay active; providers are still polled and native watches still fire.
- `Status()` shows `Pinned: true`, `Snapshot` at the pinned version, and `Live`
  at the newest validated version, so divergence is visible.
- Validation failures while pinned still reach `OnError`, so a broken config is
  not silently masked by the pin.

`Unpin()` applies the newest validated snapshot and emits exactly one `Change`
whose `Fields` is the accumulated diff between the pinned and live snapshots.
This preserves the invariant that `OnChange` observes every applied transition:
handlers see one coalesced event rather than a replay of each skipped step.

`Pin` on an already-pinned watcher repins to the new version. `Unpin` on an
unpinned watcher is a no-op. Both are safe for concurrent use; they set an
`atomic.Pointer[pinState]` that the reconciler goroutine reads in `flush`.

With the default `WithHistory(0)`, only `PinCurrent` and `Pin(currentVersion)`
succeed; older versions return `ErrNoSuchSnapshot`. This is the intended default
operational move ("freeze config, I am debugging") and costs no extra retention.

---

## 10. Workstream E: source chains

This is the only change to core resolution semantics, which is why it lands after
workstream B can observe its behavior.

### 10.1 Tag syntax

```go
Port     int           `source:"env:PORT,aws-ps://svc/port" default:"8080"`
DBPass   secret.String `source:"env:DB_PASS,aws-sm://prod/db#password" onfail:"fail"`
LogLevel string        `source:"env:LOG_LEVEL" default:"info"`   // unchanged, single ref
```

### 10.2 Parsing

Naive comma splitting is wrong: commas occur inside query options
(`?tags=a,b`) and inside opaque `exec:` paths. `ParseRefs` splits on a comma only
when the remainder begins with a scheme-like token matching
`^[a-zA-Z][a-zA-Z0-9+.\-]*:`. To embed a literal comma that would otherwise be
mistaken for a separator, percent-encode it as `%2C`; this is documented in the
ref grammar section of `concepts.md` and enforced by tests covering
`exec:echo a,b`, `?tags=a,b`, and `exec:echo foo,bar:baz` (the known ambiguous
case, which requires encoding).

`ParseRef` is retained unchanged for single-ref parsing and external callers.

### 10.3 Resolution

`fieldSpec.Ref Ref` becomes `fieldSpec.Refs []Ref`. Resolution walks the chain in
declaration order:

1. Entry resolves to a value → that entry wins; stop.
2. Entry returns `ErrNotFound` → fall through to the next entry.
3. Entry returns any other kind → stop the walk and apply the field's `onfail`
   policy. Later entries are **not** tried, because a chain expresses precedence,
   not failover (decision D2); silently sliding down to a lower-precedence source
   because a higher one had a transient outage would make config non-deterministic
   under partial failure.
4. All entries return `ErrNotFound` → apply `default:` if present, else
   `optional` handling, else fail. This is exactly today's behavior for a
   single-ref field.

Availability fallback remains `middleware.Failover`, composed per provider.

### 10.4 Failure policy

```go
`onfail:"keeplast"`  // default
```

| Policy | Behavior |
|--------|----------|
| `keeplast` | Retain the last applied value for this field; deliver the error to `OnError`; let other fields' changes apply. If there is no previously applied value (initial `Load`), **fail**. It does NOT fall back to `default:`, because `default:` applies only to genuine absence (`ErrNotFound`), never to an error: silently masking a permission-denied or unreachable-backend error with a dev default is the footgun the `exec:`/not-found decision already rejects. To use the default on an error, opt in explicitly with `onfail:"default"`. |
| `default` | Use the `default:` tag value. Rejected at spec-walk time if the field has no `default:`. |
| `fail` | Reject the entire candidate snapshot, exactly as a validation failure does. No config update applies while this field is broken. Delivered to `OnError`. |

`keeplast` is the default because it is precisely today's behavior: an error goes
to `OnError`, the last value is retained, and the engine continues
(`reconciler.go:375-385`). Existing structs are therefore unaffected.

### 10.5 Watch behavior

Every entry in a chain is watched, not just the winner. `start`
(`reconciler.go:147-188`) currently creates one watch source per spec; it becomes
one per `(spec, ref)` pair, tagged with both indices. On any update from any
entry, the winner is recomputed by re-walking the chain against the latest
observed value for each entry.

This makes precedence live: exporting `PORT` at runtime takes over from the
remote value, and unsetting it falls back. The cost is more watchers, and for
polled providers more API calls. This is documented, and a chain entry can set
`?debounce=` or a longer poll interval per ref as it can today.

### 10.6 Ripple

`fieldSpec.Ref` is referenced by `resolve.go` (`resolveAll` scheme grouping,
`resolveOne`, `resolveBatchScheme`), `poll.go`, `reconciler.go` (`start`,
`debounceFor`, `handleErr`, `schemeForPath`), and `decode.go`. `BatchProvider`
grouping now groups by `(scheme, chain position)`. `schemeForPath` returns the
winning entry's scheme, so existing metric labels stay meaningful.

`mamorivet` (`cmd/mamori/internal/vetcheck`) parses `source` tags and must learn
chains, otherwise it silently stops flagging sensitive refs in multi-ref tags.
This is a correctness fix, not an enhancement: a vet analyzer that quietly
under-reports is worse than one that errors.

---

## 11. Workstream F: CLI

New module `cmd/mamori` with its own `go.mod`. Sole non-stdlib dependency:
`golang.org/x/tools/go/packages`. No provider modules, no cloud SDKs.

The CLI has two halves that never mix. **Static commands** read source code and
need nothing running. **Live commands** are thin clients of a running process's
admin endpoint (workstream B) and need no provider modules, because the process
being inspected already did the resolving. This is what lets `doctor` exist in
the CLI without violating decision D1: it never resolves anything itself.

### 11.1 Static commands

| Command | Behavior |
|---------|----------|
| `mamori explain ./...` | Statically extract every `source:` ref from config structs in the package pattern. Table by default, `--json` for machine consumption. Columns: field path, type, chain, default, optional, sensitive. |
| `mamori schema ./...` | Emit JSON Schema derived from field types plus `validate:` tags (`oneof`, `gte`, `lte`, `required`, `min`, `max`). |
| `mamori policy ./... --format=<f>` | Emit a least-privilege artifact from the refs. Formats: `aws-iam` (policy document scoped to the referenced secret and parameter ARNs), `gcp` (roles and resource names), `external-secret` (an ExternalSecrets `ExternalSecret` manifest). |

Struct discovery: a struct qualifies if any field carries a `source` tag.
`--type=Config` narrows to a named type when a package has several.

Tag parsing is shared with `mamorivet` rather than duplicated. The shared logic
lives in `cmd/mamori/internal/sourcetag`, an internal package consumed by both,
so chain support and the sensitivity heuristics cannot drift between the
analyzer and the CLI.

### 11.2 Live commands

```bash
mamori doctor --endpoint https://svc.internal:9090 --bearer-file /run/tok
mamori doctor --endpoint unix:///run/mamori-admin.sock
mamori status --endpoint ... --watch          # re-render on an interval
```

| Command | Behavior |
|---------|----------|
| `mamori doctor` | `GET /` on the admin endpoint, render the `Report` as a table, exit non-zero if unhealthy. `--json` passes the body through unchanged. |
| `mamori status` | The same report rendered for humans, with `--watch` to re-poll on an interval for a live view during a rollout or rotation. |

Endpoint forms match the server's: `https://host:port`, `unix:///path.sock`, and
`http://host:port` only with `--insecure`. Auth flags mirror the shipped
`Authenticator` schemes: `--bearer` / `--bearer-file`, `--basic user:pass` /
`--basic-file`, `--client-cert` and `--client-key` for mTLS. File and stdin
variants exist for every credential so tokens never have to appear in a shell
history or a process listing.

**Detecting that the admin API is off** is a first-class outcome, not an error
case to squint at. The three states are distinguished explicitly and given
distinct exit codes, because a script must be able to tell "config is broken"
from "I could not see the config":

| Exit | Meaning |
|------|---------|
| 0 | Report fetched, every field healthy |
| 1 | Report fetched, one or more fields unhealthy |
| 2 | Endpoint reachable but not a mamori admin API, or admin API disabled |
| 3 | Endpoint unreachable: connection refused, no such socket, TLS failure |
| 4 | Reachable and is a mamori admin API, but authentication failed |

Exit 2 is inferred from a connection that succeeds while `/` returns 404 or a
body that is not a `Report`, which is exactly what a process running without
`WithAdminHTTP` on some other port looks like. The message says so in those
words and points at `WithAdminHTTP`, rather than reporting a parse failure.

`mamori doctor --compare ./...` cross-checks the two halves: it fetches the live
report and diffs its field set against the refs declared in the source tree,
flagging fields that exist in one and not the other. That catches a deployed
binary lagging the source it was built from, which is otherwise invisible. It is
opt-in because it is the only live command that also needs a source tree.

Because the CLI consumes it, the admin endpoint's `Report` JSON is a public
contract, not an internal debug shape. It is versioned with the same `/v1/`
discipline as the server protocol in 13.4, and the same additive-only rule
applies.

Released via the existing GoReleaser config, which gains a second build target.

---

## 12. Workstream G: documentation

### 12.1 New site pages

| Page | Content |
|------|---------|
| `site/src/pages/docs/observability.md` | `Status`, `Health`, `Doctor`, both HTTP exposure modes (`Handler` on your own mux, `WithAdminHTTP` for its own server), when to pick which, and the readiness-probe recipe. States plainly that this endpoint never serves values and points at `server.md` for the surface that does. Cross-links `opentelemetry.md`. |
| `site/src/pages/docs/testing.md` | `mamoritest`, the CI preflight pattern with `Doctor`, and `NewFakeClock` for poll-interval tests. |
| `site/src/pages/docs/cli.md` | Static commands (`explain`, `schema`, `policy`) and live commands (`doctor`, `status`), the exit-code table, auth flags, install instructions, and the CI-versus-incident split between `mamori.Doctor` and `mamori doctor`. |
| `site/src/pages/docs/auth.md` | The `Authenticator` interface, the shipped schemes, `x/authjwt`, and writing your own. Shared by the admin endpoint and the server. |
| `site/src/pages/docs/server.md` | Why fan-out, bindings, transports, policy, audit, the wire protocol reference, and a worked sidecar deployment. Must state the blast-radius tradeoff, not only the benefits. |
| `site/src/pages/docs/providers/mamori.md` | The `mamori://` client, endpoint forms, and the note that it is a native watch. |

### 12.2 Updated pages

| Page | Change |
|------|--------|
| `docs/index.md` | Nav entries for the three new pages |
| `docs/concepts.md` | Chain grammar, precedence rules, `onfail`, snapshot versioning, pin |
| `docs/usage.md` | Chain and `onfail` examples; `Status`/`Pin` in the watch walkthrough |
| `docs/security.md` | The two-surface model and why it is two: admin serves metadata and can never serve values, the server serves values under policy and audit. The unauthenticated default of `WithAdminHTTP` and the recommendation to bind it to localhost or a non-ingress port. Ref redaction denylist. History secret-retention tradeoff. Server blast radius. |
| `docs/writing-a-provider.md` | Error-kind mapping table, the `Fail` hook, the new required conformance case, and the "do not break the `errors.Is` chain" rule |
| `docs/cli/vet.md` | Chain-aware analysis |
| `docs/providers/*.md` (31 pages) | Per-provider error-mapping table: backend error → mamori kind |
| `docs/providers/index.md` | Conformance badge now covers error classification; `mamori://` added to the provider table as a native watch |
| `docs/index.md` (non-goals) | The section 3 restatement: still not a secrets store, and why the server does not contradict that |
| `docs/comparison.md` | Status, doctor, and precedence chains against viper/koanf/runtimevar |
| `docs/opentelemetry.md` | New `mamori.error.kind` attribute |

### 12.3 Repository docs

- `README.md`: new sections for observability, testing, auth, the server, and the
  CLI; provider table gains an error-classification column and a `mamori://` row.
  The "What makes it different" list gains fan-out.
- `server/README.md`, `providers/mamori/README.md`, `x/authjwt/README.md`.
- 31 provider `README.md` files: error-mapping table.
- `x/otel/README.md`: the new attribute.
- `CONTRIBUTING.md`: error mapping is now part of the provider checklist.

---

## 13. Workstream H: config server

A read-through fan-out of upstream providers. One process holds the cloud
credentials and maintains one upstream watch per published value; many local
processes consume it. The wins are concrete: credentials live in one place,
N consumers stop making N times the API calls against metered backends, and
workloads that cannot hold their own IAM role still get config.

It shares no code path and no port with the admin endpoint of workstream B. That
separation is deliberate: one surface deliberately redacts, the other
deliberately does not, and a shared path is one typo away from a leak.

### 13.1 Deployment model

A library plus your own binary (decision D10), so the deployed artifact contains
only the providers you import:

```go
import (
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/server"
	_ "github.com/xavidop/mamori/providers/aws"
	_ "github.com/xavidop/mamori/providers/vault"
)

srv, err := server.New(
	server.Bind("db-password", "aws-sm://prod/db#password"),
	server.Bind("api-key",     "vault://kv/data/api#key"),
	server.BindFile("/etc/mamori/bindings.yaml"),   // or declare them in YAML

	server.WithAuth(mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{1001}})),
	server.WithPolicy(server.PrefixPolicy(map[string][]string{
		"uid:1001": {"db-*"},
	})),
	server.WithAudit(logger),

	server.Unix("/run/mamori.sock", 0600),
	server.TCP(":8443", server.TLS(tlsCfg)),
)
if err != nil { return err }
defer srv.Close()
return srv.Serve(ctx)
```

`server/` is its own module depending only on core and stdlib, matching the
monorepo's per-module release cadence. It links no providers itself; they arrive
through the registry exactly as they do for `Load` and `Watch`.

### 13.2 Bindings

The server publishes a fixed, operator-declared name-to-ref table. It never
resolves a ref supplied by a client (decision D9).

```yaml
bindings:
  db-password: aws-sm://prod/db#password
  api-key:     vault://kv/data/api#key
  tls-cert:    file:///etc/tls/tls.crt
```

This is the single most important security property of the whole workstream. A
server that resolved client-supplied refs would hand every authenticated client
`file:///etc/shadow` as an arbitrary file read and `exec:` as remote command
execution. Bindings delete that class outright rather than mitigating it, and
they decouple clients from backends: moving a value from AWS to Vault is a
server-side edit that no client notices.

Two guards on the table itself:

- `exec:` bindings are rejected unless the server is constructed with
  `server.AllowExec()`, mirroring how `exec:` is already opt-in in core
  (`builtin_exec.go:43`).
- `mamori:` bindings are rejected unless `server.AllowChaining()` is set, since a
  binding pointing at the server itself is an infinite loop, and a cycle across
  two servers is worse. Chaining is legitimate for tiered topologies, so it is
  available, but not by accident.

### 13.3 Upstream watching

The server holds one watch per binding and serves every client from the
reconciled result. This is where the fan-out economics come from: one hundred
consumers of `db-password` produce one upstream poll, not one hundred.

This requires core to export the watch-source selection currently inlined in
`engine.start` (`reconciler.go:147-164`), which picks a native
`WatchableProvider.Watch` and falls back to `pollWatch`:

```go
// WatchRef returns a channel of updates for ref, using the provider's native
// watch when available and polling otherwise.
func WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update
```

Extracting it is a refactor core wants regardless, since the same selection logic
is what `Doctor` and the engine both reason about.

A binding whose upstream is failing serves its last known good value and reports
the upstream error kind in the response metadata, so a client can distinguish
"fresh" from "stale but serving".

### 13.4 Wire protocol (v1)

JSON over HTTP, identical on TCP and UDS.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/v1/values/{name}` | resolve one binding |
| `POST` | `/v1/values` | batch resolve, body `{"names":[...]}`, backs `BatchProvider` |
| `GET` | `/v1/watch?name=a&name=b` | SSE stream of updates |
| `GET` | `/v1/healthz` | server liveness, no binding detail |

```json
{
  "name": "db-password",
  "bytes": "aHVudGVyMg==",
  "version": "AWSCURRENT-abc123",
  "sensitive": true,
  "not_after": "2026-07-24T12:00:00Z",
  "metadata": {}
}
```

The body maps one-to-one onto `mamori.Value`, with `bytes` base64-encoded because
values are arbitrary bytes, not necessarily UTF-8.

Errors carry the workstream A classification, which is the payoff that makes the
client transparent:

```json
{"error": {"kind": "permission_denied", "message": "..."}}
```

| Kind | Status |
|------|--------|
| `not_found` | 404 |
| `invalid` | 400 |
| `unauthenticated` | 401 |
| `permission_denied` | 403 |
| `rate_limited` | 429 |
| `unavailable` | 503 |
| `unknown` | 500 |

Because the kind travels on the wire, a `mamori://` client reproduces the
upstream classification exactly, so `Doctor` against a fan-out server reports
`permission_denied` on the real AWS secret rather than a generic proxy failure.

SSE frames are `event: update` with the same body, `event: error` with the error
body, and a comment heartbeat on an interval to survive idle-connection reaping
by proxies.

Every value response sets `Cache-Control: no-store`.

### 13.5 Transports

The same `http.Handler` on different listeners (decision D11):

```go
func Unix(path string, mode os.FileMode) Option   // 0600 recommended
func TCP(addr string, opts ...TCPOption) Option
func TLS(cfg *tls.Config) TCPOption
```

`Unix` removes a stale socket file at the path on start, creates it with the
given mode, and unlinks it on `Close`. `TCP` without `TLS` fails construction
(decision D12) unless `server.InsecureNoTLS()` is passed, which is named to be
uncomfortable to type and to grep for in review.

Both transports may be enabled at once on one server: a UDS for local workloads
and a TLS TCP listener for remote ones, serving identical bindings under the
identical policy. `Serve` runs every configured listener and returns when the
first fails; `Close` shuts down all of them and unlinks any socket file.

### 13.6 Authentication

`server.WithAuth` takes the same `mamori.Authenticator` defined in 7.6. There is
no server-specific auth layer and no second interface: every scheme that works on
the admin endpoint works here unchanged, including anything you or a third party
implement.

| Scheme | UDS | TCP + TLS | Notes |
|--------|-----|-----------|-------|
| `PeerCred` | **yes** | no | Kernel-verified uid/gid. Denies on TCP. The best fit for a sidecar. |
| `MTLS` | no | **yes** | Requires `ClientAuth: tls.RequireAndVerifyClientCert`. The best fit for cross-host. |
| `BearerToken`, `APIKey` | yes | yes | Pair with TLS on TCP; the token is a bearer credential. |
| `BasicAuth` | yes | yes | Same caveat. |
| `authjwt.New` | yes | yes | For an existing IdP. |
| `AnyOf` / `AllOf` | yes | yes | e.g. `AnyOf(PeerCred(...), MTLS(...))` across both listeners. |

Unlike the admin endpoint, **authentication is mandatory**: `server.New` returns
an error when no `Authenticator` is configured. The admin endpoint can default to
open because it serves only metadata; this one serves real secret values, so the
open case has to be written down. `server.NoAuth()` exists for the genuinely
trusted case, a UDS at mode 0600 where file permissions are the access control,
and it is refused on a TCP listener outright.

The same principle as the policy applies: the permissive choice is available, but
it must appear in your source rather than arrive by omission.

### 13.7 Authorization

Authentication says who the caller is; authorization says which names they may
read. The `Authenticator` supplies the `Identity`; the policy consumes it.

```go
type Policy interface {
	Allow(id mamori.Identity, name string) error
}

func AllowAll() Policy                                    // explicit, for a trusted local sidecar
func PrefixPolicy(m map[string][]string) Policy           // subject -> name globs
func PolicyFunc(fn func(mamori.Identity, string) error) Policy
```

`server.New` returns an error when no policy is configured. There is no implicit
default, because the plausible one ("allow anything the caller is authenticated
for") turns a single leaked credential into every secret the server can reach.
`AllowAll()` exists so that choice is written down in the source rather than
inherited by omission.

A denied name returns 403 with kind `permission_denied` and is indistinguishable
from a name that does not exist, so the policy does not double as a directory of
what other clients can read.

### 13.8 Audit

`server.WithAudit(logger)` logs every request: identity subject, binding name,
allow or deny, resulting kind, and latency. It never logs values. This is the
same concern `middleware.Audit` already covers for provider calls and reuses its
formatting conventions.

---

## 14. Workstream I: `providers/mamori` client

A provider module like any other, so a consumer's config struct does not know or
care that it is talking to a fan-out server.

```go
type Config struct {
	DBPassword secret.String `source:"mamori://db-password"`
	APIKey     secret.String `source:"mamori://api-key"`
}

cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(
	mamoriprov.New(mamoriprov.Config{
		Endpoint: "unix:///run/mamori.sock",
		// or "https://config.internal:8443" with Auth and TLS
	}),
))
```

The ref path is a binding name, never an upstream ref, which is the client-side
half of decision D9.

Endpoint forms: `unix:///path/to.sock` (a custom `http.Transport.DialContext`),
`https://host:port`, and `http://host:port` only when the client is constructed
with `InsecureNoTLS`, matching the server's posture.

Interfaces implemented:

- `Provider` via `GET /v1/values/{name}`
- `BatchProvider` via `POST /v1/values`, so a struct with twenty bindings makes
  one request rather than twenty
- `WatchableProvider` via the SSE stream, so `mamori://` is a **native** watch in
  the provider table, with automatic reconnection and resubscription on
  disconnect, backed off and jittered

Classification passthrough is a hard requirement: the client reconstructs the
sentinel from the wire `kind` so `errors.Is(err, mamori.ErrPermissionDenied)`
holds for a consumer exactly as it would against AWS directly. This is precisely
what the workstream A conformance case tests.

`Value.Sensitive` is set from the wire field, so a value that was a secret
upstream stays a secret through the hop and continues to redact.

Conformance is unusually strong here: `providertest` runs against a real
in-process `server.Server` fronting an in-memory upstream provider, so the client,
the wire protocol, and the server are all exercised by the same kit that
validates every other provider.

---

## 15. Testing strategy

| Workstream | Tests |
|------------|-------|
| A | `ErrorKind` table over wrapped, joined, and nil errors; `ProviderError` unwrap reaches sentinels; `providertest` `ErrorClassification` case; 31 per-provider SDK mapping tables |
| B | Report construction from a driven engine; `Age`/`Stale` recomputation against `FakeClock`; redaction of denylisted opts; concurrent `Status` under `-race` while the engine reconciles; handler routes and the 503 shape; a test asserting the route set is exactly `/` and `/healthz`, so no future change can add a value-bearing route without failing; a test seeding a distinctive secret into the config and asserting it appears nowhere in any admin response body; `Doctor` against `mamoritest` providers seeded with each failure kind. For `WithAdminHTTP`: no listener and no extra goroutine without the option (asserted with `goleak` plus a connect attempt on a pre-chosen port); a bind failure fails `Watch` and leaves no watcher running; `Close` releases the port, verified by rebinding the same address afterward |
| C | The kit tests itself: `Set`/`Del`/`Fail` drive a real `Watch`; `WaitForSnapshot` fails cleanly on timeout; `goleak` on kit teardown |
| D | Version monotonicity; `History` bounds and ordering at n=0, 1, and many; pin blocks apply while `Live` advances; `Unpin` emits exactly one coalesced `Change` with the correct accumulated diff; validation errors still surface while pinned; concurrent `Pin`/`Unpin` under `-race` |
| E | `ParseRefs` on the comma-ambiguity corpus; chain walk for win, not-found fallthrough, non-not-found stop, and all-not-found-to-default; each `onfail` policy including `keeplast` with no prior value; live precedence takeover and fallback under watch; `BatchProvider` grouping with chains; `mamorivet` on chained sensitive refs |
| F | Golden-file tests for `explain`, `schema`, and each `policy` format against fixture packages in `testdata`. For the live half: each exit code against a purpose-built fixture, including a real `Handler` for 0 and 1, a bare 404 mux for 2, a closed port and a missing socket path for 3, and a `Handler` with auth for 4; endpoint parsing for all three URL forms; credential flags read from file and stdin without appearing in `os.Args`; `--compare` detecting both an extra and a missing field |
| H | Binding table rejects `exec:` and `mamori:` without the opt-ins; `New` errors with no policy configured and with no authenticator configured; `NoAuth` is refused on a TCP listener and accepted on a UDS; both listeners serve identical bindings under one policy, and `Close` shuts down both; TCP without TLS errors unless `InsecureNoTLS`; kind-to-status mapping table; denied and nonexistent names are byte-identical responses; `Cache-Control: no-store` on every value response; SSE delivers updates and survives a forced disconnect with resubscription; one upstream watch serves N concurrent clients (asserted by counting resolves against a fake); UDS file mode is honored and the socket is unlinked on `Close`; `PeerCred` denies on TCP and on unsupported platforms; audit log never contains a value, asserted by scanning captured output for the seeded secret |
| I | `providertest` against a real in-process server over both UDS and TLS TCP; `errors.Is` reaches the correct sentinel for every kind returned by the server; `Value.Sensitive` survives the hop; batch path issues one request for a multi-binding struct; reconnect backoff on server restart mid-watch; endpoint parsing for all three URL forms including the `http://` refusal without `InsecureNoTLS` |

Existing invariants that must not regress, and which need explicit regression
tests given the size of the change: `Get` never returns a partially-applied or
validation-failing snapshot; `OnChange` is delivered serially on a single
goroutine; the drop-oldest queue policy holds; `goleak` passes on `Close`.

Race detector on for the whole core suite, as today.

## 16. Backward compatibility

Source-compatible for application authors. Every addition is a new symbol, a new
optional tag, or a new `Option`. Single-ref `source` tags parse identically, and
`onfail`'s default reproduces current behavior exactly.

Two breaking changes, both confined to provider authors:

1. `providertest.Config.Fail` is required. In-repo providers are updated here;
   external providers break on upgrade until they add the hook.
2. `fieldSpec` is unexported, so its change is internal, but any provider
   relying on `ProviderError.Err` being the raw SDK error will now find it
   wrapped in a sentinel. `errors.As` to the SDK type still works.

Both belong in the CHANGELOG under a clearly marked provider-authors heading.

Workstreams H and I introduce no compatibility surface of their own: `server`,
`providers/mamori`, and `x/authjwt` are new modules at v0, and nothing existing
imports them. The one thing that hardens on first release is the `/v1/` wire
format, per the risk noted in section 18.

## 17. Build order

1. **A** error kinds: sentinels, `ErrorKind`, `providertest.Fail` and the
   conformance case. Providers still compile; the new case fails until updated.
2. **A'** provider sweep: 31 modules, mapping plus table tests.
3. **B** status, health, doctor, handler, `Authenticator` and the core schemes.
4. **C** `mamoritest`, which depends on `Status().Snapshot`.
5. **D** history and pin.
6. **E** source chains, including the `mamorivet` fix.
7. **H** config server, including the `WatchRef` extraction in core.
8. **I** `providers/mamori` client, whose conformance suite validates H.
9. **F** CLI, including the shared tag-parsing extraction.
10. **G** docs sweep. `x/authjwt` lands alongside, being small and independent.

Steps 1 and 2 gate everything. Step 6 is the highest-risk change to existing
behavior and is deliberately sequenced after the introspection work that makes
it observable. Steps 7 and 8 are the largest additions but touch no existing
code path beyond the `WatchRef` extraction, so they carry regression risk far
below their size. Step 2 is wide but mechanical and parallelizable across
modules.

This spec is well past the size of a single implementation plan. It decomposes
along the same seams: (1) A + A', error kinds and the provider sweep;
(2) B + C, introspection, auth, and the test kit, which share the report type;
(3) D, history and pin; (4) E, source chains; (5) H, the server; (6) I, the
client provider; (7) F + G, CLI and docs. Each plan leaves the tree green, so
work can stop cleanly at any boundary.

## 18. Open risks

- **Chain watch cost.** Watching every chain entry multiplies polling against
  paid cloud APIs. Mitigated by per-ref `?debounce=` and documented explicitly;
  not solved. If it proves painful, a future `?watch=false` per-entry opt would
  let an entry be resolved once at startup without a watcher.
- **Comma ambiguity in `exec:`.** `exec:echo foo,bar:baz` cannot be
  disambiguated without encoding. `exec:` is already opt-in
  (`builtin_exec.go:43`); the requirement is documented and tested rather than
  worked around.
- **Sweep drift.** Thirty-one mapping tables can rot as SDKs add error codes.
  The conformance case catches chain-breakage but not a missing new error code.
  Accepted; `KindUnknown` degrades honestly.
- **The server is a new blast radius.** It holds credentials for every backend it
  fronts, so compromising it is worse than compromising any single consumer. The
  mitigations are structural rather than incidental: no client-supplied refs, a
  mandatory explicit policy, no persistence, and mandatory TLS on TCP. It is
  still a genuine concentration of risk and the docs must say so plainly rather
  than only selling the fan-out benefits.
- **Wire protocol compatibility.** `/v1/` is versioned from the first commit, but
  once external clients exist the response shape is effectively frozen. Adding
  fields is safe; changing `bytes` encoding or error shape is not. This is worth
  a deliberate review before the first tagged release of `server`.
- **Peer-credential portability.** `PeerCred` is meaningful only on Linux and
  Darwin. The Windows build denies unconditionally, which is safe but means a
  Windows deployment must use a different authenticator. Documented, not solved.
- **Fan-out staleness.** A consumer's freshness is now bounded by the server's
  upstream poll interval plus its own, so `WithStale` thresholds that were tuned
  against a direct backend may need revisiting behind a server. Surfaced through
  the response metadata so `Status` can show it, but it is a real behavior change
  for anyone migrating a direct provider to `mamori://`.
