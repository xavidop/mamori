---
layout: ../../../layouts/DocsLayout.astro
title: Auth schemes
---

# Auth schemes

The schemes mamori ships. Each is a `mamori.Authenticator`, so it works unchanged on the admin endpoint and the [config server](/docs/server/). Attach any of them with `WithAuth` (see the [overview](/docs/auth/)). For rotation and writing your own, see [Custom authenticators](/docs/auth/custom/).

## BasicAuth

```go
func BasicAuth(user string, pass secret.String) Authenticator
```

```go
auth := mamori.BasicAuth("admin", secret.NewString("hunter2"))
```

Checks HTTP Basic credentials against a fixed user and password. Both the username and the password are compared in constant time. `pass` is a `secret.String`, so it redacts in logs and error values. Implements `Challenger` (`Basic realm="mamori"`).

## BearerToken

```go
func BearerToken(token secret.String) Authenticator
```

```go
auth := mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN")))
```

Checks `Authorization: Bearer <token>` against a fixed token, compared in constant time. The `Bearer ` prefix itself is checked with `strings.HasPrefix`, since it is a fixed, public protocol string. Implements `Challenger` (`Bearer`).

## APIKey

```go
func APIKey(header string, key secret.String) Authenticator
```

```go
auth := mamori.APIKey("X-API-Key", secret.NewString(os.Getenv("ADMIN_KEY")))
```

Checks a named header against a fixed key, compared in constant time. Implements no `Challenger`: an API key is not a scheme a generic HTTP client knows how to answer a `WWW-Authenticate` challenge for, so a failed request gets a bare `401`.

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

Authenticates by the client's already-verified TLS certificate. It requires the server be configured with `tls.RequireAndVerifyClientCert` (via `WithAdminTLS`); `MTLS` only checks which verified identity is allowed, not whether the chain is trustworthy (the Go TLS stack has already done that). `AllowedCNs`/`AllowedDNSNames` are optional allowlists checked against the leaf certificate's `CommonName` and DNS SANs; if both are empty, any verified certificate is accepted. On a non-TLS connection, or a TLS connection with no client certificate, `MTLS` denies every request.

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

Authenticates a Unix-domain-socket peer by the uid/gid the kernel reports at accept time (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` via `GetsockoptXucred` on Darwin), never anything the client presents. Because the identity comes from the kernel, it cannot be spoofed by a client that can merely connect to the socket.

`UIDs`/`GIDs` are optional allowlists, ORed together (a peer is permitted if its uid is in `UIDs` or its gid is in `GIDs`). If both are empty, any peer whose credentials were read is permitted. On success, `Identity.Subject` is `"uid:<uid>"` and `Attrs` carries `"uid"`, `"gid"`, and `"pid"` (Darwin's `Xucred` carries no pid, so `Attrs["pid"]` is always `["0"]` there).

`PeerCred` requires the [config server](/docs/server/)'s `Unix(...)` transport. `WithAdminHTTP` does not support it: it only listens on TCP, where there is no Unix-socket peer to read credentials from. It also denies outright when no peer credentials are available, and on any platform other than Linux or Darwin.

## JWT (x/authjwt)

JWT support ships as a separate module, `github.com/xavidop/mamori/x/authjwt`, since it depends on `github.com/golang-jwt/jwt/v5` and core takes no non-stdlib dependencies. The returned value is a `mamori.Authenticator` like any other.

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

```sh
go get github.com/xavidop/mamori/x/authjwt
```

Key material comes from exactly one of `Key` or `Keyfunc`. `Key` is the normal path: each helper supplies both the key and the algorithms it is valid for, so the two can never drift apart.

```go
func HMAC(secret []byte) KeyOption          // HS256, HS384, HS512
func RSAPublicKey(key *rsa.PublicKey) KeyOption      // RS256/384/512, PS256/384/512
func ECDSAPublicKey(key *ecdsa.PublicKey) KeyOption  // ES256, ES384, ES512
func EdDSAPublicKey(key ed25519.PublicKey) KeyOption // EdDSA
```

`Keyfunc` is the escape hatch (most commonly a JWKS endpoint that picks a key by the token's `kid`). Because a raw `Keyfunc` can return key material for any algorithm, `Algorithms` must also be set explicitly; leaving it empty is a `Config` error, not a permissive default.

`Issuer` (when set) must match the token's `iss` exactly; `Audiences` (when non-empty) requires `aud` to contain at least one listed value. `SubjectClaim` (default `"sub"`) names the claim copied into `Identity.Subject`. `Claims` names claims copied into `Identity.Attrs`; the `scope`/`scp` claims are split on spaces even when string-valued. `Realm`, if set, appears in the challenge.

Security posture, enforced on every request: parsing is restricted with `jwt.WithValidMethods` to exactly the algorithms implied by the key (so `alg: none` and the RSA/HMAC key-confusion attack are rejected); expiration is mandatory (`jwt.WithExpirationRequired` rejects an expired token and one with no `exp`); issuer/audience are validated when configured; and the token is read only from the `Authorization` header with a case-insensitive `Bearer ` prefix. A missing, malformed, expired, or invalid token is a `401`, never `ErrForbidden`. The authenticator implements `Challenger`.

## AnyOf and AllOf

```go
func AnyOf(as ...Authenticator) Authenticator
func AllOf(as ...Authenticator) Authenticator
```

`AnyOf` allows a request if any member allows it, for example a static admin token or mTLS from a mesh sidecar:

```go
auth := mamori.AnyOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{}),
)
```

Every member is evaluated on every request, even after one succeeds or fails, so total work never depends on which member matched (no timing oracle). If any member implements `Challenger`, the first such member in argument order determines `AnyOf`'s challenge.

`AllOf` allows a request only if every member allows it, for example a bearer token and an mTLS-verified identity:

```go
auth := mamori.AllOf(
	mamori.BearerToken(secret.NewString(os.Getenv("ADMIN_TOKEN"))),
	mamori.MTLS(mamori.MTLSOptions{AllowedCNs: []string{"mesh-sidecar"}}),
)
```

The first denial fails the whole check and later members are skipped. The `Identity` of the first member is returned; by convention that first member is the primary authenticator and later members perform supplementary checks.
