---
layout: ../../layouts/DocsLayout.astro
title: Auth
---

# Auth

The admin HTTP endpoint (`Handler`, `WithAdminHTTP` - see [HTTP exposure](../observability#http-exposure)) has no `Authenticator` by default: any request that can reach it gets the `Report`. `WithAuth` attaches one. mamori ships several schemes for common cases, plus `Func` variants that support live credential rotation, and the interface itself is small enough to implement your own.

## The `Authenticator` interface

```go
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}
```

A `nil` error allows the request; any other error denies it.

```go
type Identity struct {
	Subject string
	Attrs   map[string][]string
}
```

`Subject` is a stable principal name; `Attrs` carries scheme-specific detail (certificate SANs, token claims, a peer uid/gid/pid). `Attrs` is multi-valued because authorization commonly needs multi-valued claims - groups, scopes, token audiences, multiple certificate SANs - and a single string per key would force a scheme to join-encode them. Both may be empty for a scheme that authenticates without naming a principal, such as a shared bearer token. The admin endpoint ignores the returned `Identity` - it only ever serves metadata, so there is nothing to authorize per-principal there. The [config server](../server) uses it: its `Policy` (`server.WithPolicy`) decides what a given caller may see based on the `Identity` an `Authenticator` returns. Because both surfaces share one interface, an `Authenticator` written for the admin endpoint works unchanged on the config server too.

Two more small pieces complete the interface:

```go
type Challenger interface {
	Challenge() string
}
```

Optional. A scheme that implements `Challenger` gets its return value sent as the `WWW-Authenticate` header on a `401`, so a browser or HTTP client knows how to prompt for credentials. A scheme that doesn't implement it produces a bare `401`.

```go
type AuthFunc func(r *http.Request) (Identity, error)
```

Adapts a plain function to `Authenticator`; `Authenticate` just calls the function.

```go
var ErrForbidden = errors.New("mamori: forbidden")
```

Return `ErrForbidden` from `Authenticate` to produce a `403` rather than a `401` - use it when the caller is authenticated but not permitted. Any other error produces a `401`.

## Wiring it up

`WithAuth` is a `HandlerOption`, so it goes wherever `HandlerOption`s go: as an argument to `Handler`, or after the address in `WithAdminHTTP`.

```go
auth := mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN")))

// Mounted on your own mux:
mux.Handle("/", mamori.Handler(w, mamori.WithAuth(auth)))

// Or with mamori's self-hosted server:
w, err := mamori.Watch[Config](ctx,
	mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
)
```

Applying `WithAuth` more than once panics rather than silently letting the second call win - compose multiple schemes explicitly with `AnyOf` or `AllOf` instead, so the intended semantics are visible at the call site.

## The shipped schemes

### BasicAuth

```go
func BasicAuth(user string, pass secret.String) Authenticator
```

```go
auth := mamori.BasicAuth("admin", secret.NewString("hunter2"))
```

Checks HTTP Basic credentials against a fixed user and password. Both the username and the password are compared in constant time, so a failed request discloses neither the password through response timing nor whether `user` is even the right username. `pass` is a `secret.String`, so it redacts in logs and error values. Implements `Challenger` (`Basic realm="mamori"`).

### BearerToken

```go
func BearerToken(token secret.String) Authenticator
```

```go
auth := mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN")))
```

Checks `Authorization: Bearer <token>` against a fixed token, compared in constant time. The `Bearer ` prefix itself is checked with a plain `strings.HasPrefix`, not constant-time - it's a fixed, public protocol string, not part of the secret. Implements `Challenger` (`Bearer`).

### APIKey

```go
func APIKey(header string, key secret.String) Authenticator
```

```go
auth := mamori.APIKey("X-API-Key", secret.NewString(os.Getenv("ADMIN_KEY")))
```

Checks a named header against a fixed key, compared in constant time. Implements no `Challenger`: an API key isn't a scheme a generic HTTP client knows how to respond to a `WWW-Authenticate` challenge for, so a failed request gets a bare `401`.

### MTLS

```go
func MTLS(opts MTLSOptions) Authenticator

type MTLSOptions struct {
	AllowedCNs      []string
	AllowedDNSNames []string
}
```

```go
auth := mamori.MTLS(mamori.MTLSOptions{
	AllowedCNs: []string{"admin-client"},
})
```

Authenticates by the client's already-verified TLS certificate; it requires the server be configured with `tls.RequireAndVerifyClientCert` (via `WithAdminTLS`), since `MTLS` only checks *which* verified identity is allowed, not whether the certificate chain is trustworthy - the Go TLS stack has already done that. `AllowedCNs`/`AllowedDNSNames` are optional allowlists checked against the leaf certificate's `CommonName` and DNS SANs; if both are empty, any verified certificate is accepted, on the theory that verification itself is the security boundary. On a non-TLS connection, or a TLS connection with no client certificate, `MTLS` denies every request - there is no fallback and no separate secret.

### PeerCred

```go
func PeerCred(opts PeerCredOptions) Authenticator

type PeerCredOptions struct {
	UIDs []int
	GIDs []int
}
```

```go
auth := mamori.PeerCred(mamori.PeerCredOptions{
	UIDs: []int{1000, 1001},
})
```

Authenticates a Unix-domain-socket peer by the uid/gid the kernel itself reports at accept time - `SO_PEERCRED` on Linux, `LOCAL_PEERCRED` (via `GetsockoptXucred`) on Darwin - never anything the client presents in the request. Because the identity comes from the kernel rather than from request content, it **cannot be spoofed by a client that can merely connect to the socket**: there is no header, token, or certificate to forge, only a real process identity the OS itself vouches for.

`UIDs`/`GIDs` are both optional allowlists, ORed together (a peer is permitted if its uid is in `UIDs` *or* its gid is in `GIDs`; they are not ANDed). If both are empty, any peer whose credentials were successfully read is permitted - the same "verification itself is the security boundary" default `MTLSOptions`' empty allowlist documents. On success, `Identity.Subject` is `"uid:<uid>"` and `Identity.Attrs` carries `"uid"`, `"gid"`, and `"pid"` (Darwin's `Xucred` carries no pid, so `Attrs["pid"]` is always `["0"]` there).

`PeerCred` denies outright, never a silent allow, in two situations:

- **No peer credentials were ever available for this request** - a non-Unix connection, or a Unix connection whose kernel credentials were never plumbed into the request context in the first place.
- **The platform is not Linux or Darwin.** mamori has no way to read kernel-verified peer credentials anywhere else, so `PeerCred.Authenticate` returns a hard, unconditional deny with a clear error on every other platform - never a fallthrough to "no restriction configured" just because the check could not be performed.

Reading a connection's peer credentials requires cooperation from whatever accepted that connection: by the time `Authenticate` runs, only the `*http.Request` and its context are in scope, and `net/http` gives no path from a request back to the `net.Conn` it arrived on. mamori bridges this with a small seam split across the core module and the [config server](../server): core exports `Ucred`, `ContextWithPeerCred`, and, per supported platform, `PeerCredFromConn`; a Unix-socket listener calls `PeerCredFromConn` once per accepted connection and stashes the result via `http.Server.ConnContext` and `ContextWithPeerCred`, so every request on that connection carries the same peer identity the kernel reported at accept time. **`WithAdminHTTP` does not wire up this seam** - it only ever listens on TCP, where there is no Unix-socket peer to read credentials from in the first place. `PeerCred` therefore requires the [config server](../server)'s `Unix(...)` transport specifically; mounting `PeerCred` on any other listener denies every request, since nothing will ever have stashed peer credentials into the context for it to find.

### AnyOf and AllOf

```go
func AnyOf(as ...Authenticator) Authenticator
func AllOf(as ...Authenticator) Authenticator
```

`AnyOf` allows a request if any member allows it - for example, a static admin token *or* mTLS from a mesh sidecar:

```go
auth := mamori.AnyOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{}),
)
```

Every member is evaluated on every request, even after one has already succeeded or failed, so the total work never depends on which member matched or how early a mismatch was found - without this, response timing could tell an attacker which scheme "almost" accepted their request. If any member implements `Challenger`, the first such member in argument order determines `AnyOf`'s own challenge.

`AllOf` allows a request only if every member allows it - for example, a bearer token *and* an mTLS-verified network identity:

```go
auth := mamori.AllOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{AllowedCNs: []string{"mesh-sidecar"}}),
)
```

The first denial fails the whole check and later members are skipped - unlike `AnyOf`, there's no timing oracle to defend against here, since a partial failure is already a total denial. The `Identity` of the first member is returned; by convention that first member is the primary authenticator, and later members perform supplementary checks whose own `Identity` is typically empty.

## Constant-time comparison and fail-closed behavior

`BasicAuth`, `BearerToken`, and `APIKey` all compare credential material with `crypto/subtle.ConstantTimeCompare`, never `==` or `bytes.Equal`. A naive comparison short-circuits on the first mismatching byte, which would let an attacker recover a valid credential one byte at a time by timing repeated requests; constant-time comparison always walks the full length of both operands, so the timing carries no information about where, or whether, they diverge. (`MTLS`'s CommonName/DNS-SAN check is an ordinary string comparison - those are public identifiers on an already-verified certificate, not secrets, so there's nothing for timing to leak.)

All three secret-based schemes also fail closed on an unconfigured credential: if the expected value is a zero `secret.String` (`IsZero()` is true), every request is denied outright, before even looking at what the caller presented. "Unset" is never treated as "no password required" - the alternative would silently open the endpoint during the window before a credential has ever been populated.

None of the failure messages echo the value the caller presented, only that the credential was missing or invalid, so a misconfigured client can't leak its guess into logs or error-reporting pipelines that aren't held to the same secrecy bar as the auth path itself.

## The `Func` variants, and credential rotation

```go
func BasicAuthFunc(fn func() (string, secret.String)) Authenticator
func BearerTokenFunc(fn func() secret.String) Authenticator
func APIKeyFunc(header string, fn func() secret.String) Authenticator
```

Each `Func` variant reads the expected credential on every request instead of freezing it at construction. That's what makes it possible to rotate the admin credential live: read it from a mamori-managed config instead of a value fixed at startup. A typical shape is a small config watched independently, with a closure that reads its current snapshot:

```go
type AdminConfig struct {
	AdminToken secret.String `source:"aws-sm://prod/admin#token"`
}

adminW, err := mamori.Watch[AdminConfig](ctx)
if err != nil {
	log.Fatal(err)
}
defer adminW.Close()

auth := mamori.BearerTokenFunc(func() secret.String {
	return adminW.Get().AdminToken
})

w, err := mamori.Watch[Config](ctx,
	mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
)
```

When the secret manager rotates `prod/admin#token`, `adminW` picks it up on its own reconcile loop (see [Loading & watching](../usage)), and the very next request to the admin endpoint is checked against the new value - no restart, and no gap where the endpoint accepts no credential at all: until the first successful resolve, `AdminToken` is a zero `secret.String`, and the fail-closed rule above denies every request rather than opening the endpoint.

## Writing your own

Anything satisfying the interface works. The simplest path is `AuthFunc`, for a check that doesn't need its own type:

```go
auth := mamori.AuthFunc(func(r *http.Request) (mamori.Identity, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host != "10.0.0.5" {
		return mamori.Identity{}, errors.New("peer not allowed")
	}
	return mamori.Identity{Subject: host}, nil
})
```

Implement `Authenticator` (and optionally `Challenger`) on a named type when the check needs its own state or its own `WWW-Authenticate` value. Whichever shape you pick, keep the two properties the shipped schemes share: compare any caller-supplied secret material with `crypto/subtle.ConstantTimeCompare` rather than `==`, and fail closed - deny the request outright - whenever the expected credential is unset or not yet populated, rather than treating "not configured" as "no credential required."

## Not yet available

JWT auth (a planned `x/authjwt` module) is **not** part of this release.
