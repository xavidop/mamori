---
layout: ../../layouts/DocsLayout.astro
title: Auth
---

# Auth

An `Authenticator` decides whether an HTTP request may proceed and says who the caller is. One interface serves both mamori surfaces, the admin HTTP endpoint (`Handler`, `WithAdminHTTP`, see [HTTP exposure](../observability#serve-the-report-over-http)) and the [config server](../server), so a scheme configured for one works unchanged on the other. mamori ships schemes for the common cases, composition operators (`AnyOf`/`AllOf`), and `Func` variants for live credential rotation; the interface is small enough to implement your own.

## Quick start

The admin endpoint has no `Authenticator` by default: any request that can reach it gets the `Report`. `WithAuth` attaches one. This wires a shared bearer token onto mamori's self-hosted admin server:

```go
auth := mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN")))

w, err := mamori.Watch[Config](ctx,
	mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
)
```

The same `auth` value drops onto your own mux, or onto the config server, unchanged:

```go
mux.Handle("/", mamori.Handler(w, mamori.WithAuth(auth)))
```

## The `Authenticator` interface

```go
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}
```

A `nil` error allows the request; any other error denies it. On success `Authenticate` returns an `Identity`:

```go
type Identity struct {
	Subject string
	Attrs   map[string][]string
}
```

`Subject` is a stable principal name; `Attrs` carries scheme-specific detail (certificate SANs, token claims, a peer uid/gid/pid) and is multi-valued so a scheme can return groups, scopes, or multiple SANs directly. Both may be empty for a scheme that authenticates without naming a principal, such as a shared bearer token. The admin endpoint ignores the `Identity` (it only serves metadata); the [config server](../server) consumes it, since its `Policy` (`server.WithPolicy`) decides what a caller may see based on it. See [How it works](#how-it-works) for why it is one interface and why `Attrs` is multi-valued.

Two optional pieces complete the interface. `Challenger` supplies the `WWW-Authenticate` header:

```go
type Challenger interface {
	Challenge() string
}
```

A scheme that implements `Challenger` gets its return value sent as `WWW-Authenticate` on a `401`, so a browser or HTTP client knows how to prompt for credentials; a scheme that does not implement it produces a bare `401`.

`ErrForbidden` selects the status code:

```go
var ErrForbidden = errors.New("mamori: forbidden")
```

Return `ErrForbidden` from `Authenticate` to produce a `403` rather than a `401`; use it when the caller is authenticated but not permitted. Any other error produces a `401`.

`AuthFunc` adapts a plain function to `Authenticator` (see [Writing your own](#writing-your-own-authenticator)):

```go
type AuthFunc func(r *http.Request) (Identity, error)
```

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

Applying `WithAuth` more than once panics rather than silently letting the second call win; compose multiple schemes explicitly with `AnyOf` or `AllOf` instead, so the intended semantics are visible at the call site.

## BasicAuth

```go
func BasicAuth(user string, pass secret.String) Authenticator
```

```go
auth := mamori.BasicAuth("admin", secret.NewString("hunter2"))
```

Checks HTTP Basic credentials against a fixed user and password. Both the username and the password are compared in constant time, so a failed request discloses neither the password through response timing nor whether `user` is even the right username. `pass` is a `secret.String`, so it redacts in logs and error values. Implements `Challenger` (`Basic realm="mamori"`).

## BearerToken

```go
func BearerToken(token secret.String) Authenticator
```

```go
auth := mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN")))
```

Checks `Authorization: Bearer <token>` against a fixed token, compared in constant time. The `Bearer ` prefix itself is checked with a plain `strings.HasPrefix`, not constant-time, since it is a fixed, public protocol string, not part of the secret. Implements `Challenger` (`Bearer`).

## APIKey

```go
func APIKey(header string, key secret.String) Authenticator
```

```go
auth := mamori.APIKey("X-API-Key", secret.NewString(os.Getenv("ADMIN_KEY")))
```

Checks a named header against a fixed key, compared in constant time. Implements no `Challenger`: an API key is not a scheme a generic HTTP client knows how to respond to a `WWW-Authenticate` challenge for, so a failed request gets a bare `401`.

## MTLS

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

Authenticates by the client's already-verified TLS certificate. It requires the server be configured with `tls.RequireAndVerifyClientCert` (via `WithAdminTLS`), since `MTLS` only checks *which* verified identity is allowed, not whether the certificate chain is trustworthy (the Go TLS stack has already done that). `AllowedCNs`/`AllowedDNSNames` are optional allowlists checked against the leaf certificate's `CommonName` and DNS SANs; if both are empty, any verified certificate is accepted, on the theory that verification itself is the security boundary. On a non-TLS connection, or a TLS connection with no client certificate, `MTLS` denies every request: there is no fallback and no separate secret.

## PeerCred

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

Authenticates a Unix-domain-socket peer by the uid/gid the kernel itself reports at accept time (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` via `GetsockoptXucred` on Darwin), never anything the client presents in the request. Because the identity comes from the kernel rather than from request content, it **cannot be spoofed by a client that can merely connect to the socket**: there is no header, token, or certificate to forge, only a real process identity the OS itself vouches for.

`UIDs`/`GIDs` are both optional allowlists, ORed together (a peer is permitted if its uid is in `UIDs` *or* its gid is in `GIDs`; they are not ANDed). If both are empty, any peer whose credentials were successfully read is permitted, the same "verification itself is the security boundary" default `MTLSOptions`' empty allowlist documents. On success, `Identity.Subject` is `"uid:<uid>"` and `Identity.Attrs` carries `"uid"`, `"gid"`, and `"pid"` (Darwin's `Xucred` carries no pid, so `Attrs["pid"]` is always `["0"]` there).

`PeerCred` denies outright, never a silent allow, in two situations:

- **No peer credentials were ever available for this request**: a non-Unix connection, or a Unix connection whose kernel credentials were never plumbed into the request context in the first place.
- **The platform is not Linux or Darwin.** mamori has no way to read kernel-verified peer credentials anywhere else, so `PeerCred.Authenticate` returns a hard, unconditional deny with a clear error on every other platform, never a fallthrough to "no restriction configured" just because the check could not be performed.

`PeerCred` requires the [config server](../server)'s `Unix(...)` transport specifically. **`WithAdminHTTP` does not support it**: it only ever listens on TCP, where there is no Unix-socket peer to read credentials from, so mounting `PeerCred` there (or on any other listener that has not stashed peer credentials) denies every request. See [How it works](#how-it-works) for the ConnContext seam that plumbs the kernel's credentials from the listener to `Authenticate`.

## JWT (`x/authjwt`)

JWT support ships as a separate module, `github.com/xavidop/mamori/x/authjwt`, rather than in core, since it depends on a JWT library (`github.com/golang-jwt/jwt/v5`) and core takes no non-stdlib dependencies. It is a `mamori.Authenticator` like any other, so it works unchanged on both the admin endpoint and the [config server](../server).

```go
import "github.com/xavidop/mamori/x/authjwt"

auth, err := authjwt.New(authjwt.Config{
	Key:       authjwt.HMAC(secretBytes),
	Issuer:    "https://issuer.example.com/",
	Audiences: []string{"mamori-admin"},
	Claims:    []string{"groups", "scope"},
})
if err != nil {
	log.Fatal(err)
}

w, err := mamori.Watch[Config](ctx,
	mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
)
```

`authjwt.New` validates its `Config` up front and returns an error for a nonsensical configuration (a missing key source, or an unrestricted algorithm set) rather than deferring that failure to the first request.

**Key material** comes from exactly one of two places:

```go
type Config struct {
	Key        authjwt.KeyOption
	Keyfunc    jwt.Keyfunc
	Algorithms []string
	// ...
}
```

- `Key`, built from one of the helpers below, is the normal path. Each constructor supplies both the key *and* the algorithms it is valid for, so the two can never drift apart, and a caller cannot accidentally leave the algorithm set open:

  ```go
  func HMAC(secret []byte) KeyOption          // HS256, HS384, HS512
  func RSAPublicKey(key *rsa.PublicKey) KeyOption      // RS256/384/512, PS256/384/512
  func ECDSAPublicKey(key *ecdsa.PublicKey) KeyOption  // ES256, ES384, ES512
  func EdDSAPublicKey(key ed25519.PublicKey) KeyOption // EdDSA
  ```

- `Keyfunc` is the escape hatch for cases the helpers don't cover, most commonly a JWKS endpoint with key rotation (pick a key by the token's `kid` header). Because a raw `Keyfunc` can return key material for any algorithm the function is willing to produce, `Algorithms` **must** also be set explicitly whenever `Keyfunc` is used; leaving it empty is a `Config` error, not a permissive default, since an unrestricted algorithm set reopens the alg-confusion vulnerability the `Key` helpers exist to close.

`Issuer`, when set, must match the token's `iss` claim exactly; `Audiences`, when non-empty, requires the token's `aud` claim to contain at least one listed value. `SubjectClaim` (default `"sub"`) names the claim copied into `Identity.Subject`. `Claims` lists additional claim names copied into `Identity.Attrs`: a string-valued claim becomes a single-element slice, a `[]string` (or JSON array of strings) becomes a multi-element slice, and the `scope`/`scp` claims are special-cased even when string-valued, split on spaces into multiple values, matching how OAuth2/OIDC encode a space-delimited scope list as one string claim. A claim listed but absent from the token, or with an unrecognized shape, is simply absent from `Attrs`, never an error. `Realm`, if set, appears in the `WWW-Authenticate: Bearer realm="..."` challenge.

**Security posture**, enforced on every request, not just at setup:

- **Algorithm allow-listing.** Parsing is restricted with `jwt.WithValidMethods` to exactly the algorithms implied by the configured key (or the explicit `Algorithms`). A token signed with `alg: none` is rejected outright, and so is a token signed with, say, HS256 when an RSA public key is configured: the classic RSA/HMAC key-confusion attack, where an attacker signs a forged token with HS256 using the server's own (public) RSA key as the HMAC secret.
- **Expiration is mandatory.** `jwt.WithExpirationRequired` rejects both an expired token and a token with no `exp` claim at all.
- **Issuer and audience** are validated whenever configured, and a mismatch is rejected.
- **Extraction is strict.** The token is read only from the `Authorization` header with a case-insensitive `Bearer ` scheme prefix; a missing header, a different scheme, or an empty token is rejected.

A missing, malformed, expired, or otherwise invalid token is unauthenticated: `Authenticate` returns a plain error, never `mamori.ErrForbidden`, so it is a `401`. The returned authenticator implements `Challenger`, sending `WWW-Authenticate: Bearer` (with a realm, if configured).

```sh
go get github.com/xavidop/mamori/x/authjwt
```

## AnyOf and AllOf

```go
func AnyOf(as ...Authenticator) Authenticator
func AllOf(as ...Authenticator) Authenticator
```

`AnyOf` allows a request if any member allows it, for example a static admin token *or* mTLS from a mesh sidecar:

```go
auth := mamori.AnyOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{}),
)
```

Every member is evaluated on every request, even after one has already succeeded or failed, so the total work never depends on which member matched or how early a mismatch was found; without this, response timing could tell an attacker which scheme "almost" accepted their request. If any member implements `Challenger`, the first such member in argument order determines `AnyOf`'s own challenge.

`AllOf` allows a request only if every member allows it, for example a bearer token *and* an mTLS-verified network identity:

```go
auth := mamori.AllOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{AllowedCNs: []string{"mesh-sidecar"}}),
)
```

The first denial fails the whole check and later members are skipped; unlike `AnyOf`, there is no timing oracle to defend against here, since a partial failure is already a total denial. The `Identity` of the first member is returned; by convention that first member is the primary authenticator, and later members perform supplementary checks whose own `Identity` is typically empty.

## Constant-time comparison and fail-closed behavior

`BasicAuth`, `BearerToken`, and `APIKey` all compare credential material with `crypto/subtle.ConstantTimeCompare`, never `==` or `bytes.Equal`. A naive comparison short-circuits on the first mismatching byte, which would let an attacker recover a valid credential one byte at a time by timing repeated requests; constant-time comparison always walks the full length of both operands, so the timing carries no information about where, or whether, they diverge. (`MTLS`'s CommonName/DNS-SAN check is an ordinary string comparison, since those are public identifiers on an already-verified certificate, not secrets, so there is nothing for timing to leak.)

All three secret-based schemes also fail closed on an unconfigured credential: if the expected value is a zero `secret.String` (`IsZero()` is true), every request is denied outright, before even looking at what the caller presented. "Unset" is never treated as "no password required"; the alternative would silently open the endpoint during the window before a credential has ever been populated.

None of the failure messages echo the value the caller presented, only that the credential was missing or invalid, so a misconfigured client cannot leak its guess into logs or error-reporting pipelines that are not held to the same secrecy bar as the auth path itself.

## The `Func` variants and credential rotation

```go
func BasicAuthFunc(fn func() (string, secret.String)) Authenticator
func BearerTokenFunc(fn func() secret.String) Authenticator
func APIKeyFunc(header string, fn func() secret.String) Authenticator
```

Each `Func` variant reads the expected credential on every request instead of freezing it at construction. That is what makes it possible to rotate the admin credential live: read it from a mamori-managed config instead of a value fixed at startup. A typical shape is a small config watched independently, with a closure that reads its current snapshot:

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

When the secret manager rotates `prod/admin#token`, `adminW` picks it up on its own reconcile loop (see [Loading & watching](../usage)), and the very next request to the admin endpoint is checked against the new value: no restart, and no gap where the endpoint accepts no credential at all. Until the first successful resolve, `AdminToken` is a zero `secret.String`, and the fail-closed rule above denies every request rather than opening the endpoint.

## Writing your own Authenticator

Anything satisfying the interface works. The simplest path is `AuthFunc`, for a check that does not need its own type:

```go
auth := mamori.AuthFunc(func(r *http.Request) (mamori.Identity, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host != "10.0.0.5" {
		return mamori.Identity{}, errors.New("peer not allowed")
	}
	return mamori.Identity{Subject: host}, nil
})
```

Implement `Authenticator` (and optionally `Challenger`) on a named type when the check needs its own state or its own `WWW-Authenticate` value. Whichever shape you pick, keep the two properties the shipped schemes share: compare any caller-supplied secret material with `crypto/subtle.ConstantTimeCompare` rather than `==`, and fail closed (deny the request outright) whenever the expected credential is unset or not yet populated, rather than treating "not configured" as "no credential required".

## How it works

**One interface, both surfaces.** `Authenticate` returns an `Identity` even though the admin endpoint never uses it. It is one interface rather than two so an `Authenticator` written for one surface works unchanged on the other: the admin endpoint discards the `Identity` (it only ever serves metadata, so there is nothing to authorize per-principal), while the config server's `Policy` authorizes against it. A single shape means no adapter and no second implementation when you move a scheme between the two.

**Why `Attrs` is multi-valued.** Authorization commonly needs multi-valued claims: groups, scopes, token audiences, or multiple certificate SANs. A single `string` per key would force every such scheme to join-encode those values and every policy to split them back apart, so `Attrs` is `map[string][]string` and each scheme returns the natural shape directly.

**Challenger and status codes.** The handler maps an `Authenticate` error to a response: `ErrForbidden` becomes a `403` (authenticated but not permitted), any other error a `401` (unauthenticated). On a `401`, if the scheme implements `Challenger`, its `Challenge()` value is sent as `WWW-Authenticate` so a client knows how to authenticate; schemes with no meaningful challenge (like `APIKey`) implement no `Challenger` and produce a bare `401`. `Challenge()` takes no request, so a composed `AnyOf` fixes its challenge at construction to the first `Challenger`-implementing member in argument order.

**The PeerCred ConnContext seam.** By the time `Authenticate` runs, only the `*http.Request` and its context are in scope, and `net/http` gives no path from a request back to the `net.Conn` it arrived on. mamori bridges this with a small seam split across the core module and the [config server](../server): core exports `Ucred`, `ContextWithPeerCred`, and, per supported platform, `PeerCredFromConn`; the config server's Unix-socket listener calls `PeerCredFromConn` once per accepted connection and stashes the result via `http.Server.ConnContext` and `ContextWithPeerCred`, so every request on that connection carries the same peer identity the kernel reported at accept time. `PeerCred.Authenticate` reads it back. That is why `PeerCred` works only on the config server's `Unix(...)` transport and denies everywhere else: nothing else stashes credentials for it to find.

## See also

- [Observability](../observability) covers the admin endpoint and its [HTTP exposure](../observability#serve-the-report-over-http).
- [Config server](../server) is the second surface, where `Policy` authorizes against the `Identity` an `Authenticator` returns.
- [Loading & watching](../usage) explains the reconcile loop a rotating credential rides on.
