# mamori JWT authenticator (`x/authjwt`)

`github.com/xavidop/mamori/x/authjwt` is a JWT `mamori.Authenticator` for
mamori's admin HTTP endpoint and the [config server](../../server). It lives
outside the core `mamori` module because it depends on a JWT library
(`github.com/golang-jwt/jwt/v5`), and the core module takes no non-stdlib
dependencies.

Because it's an ordinary `mamori.Authenticator`, it works unchanged wherever
that interface is accepted - `mamori.WithAuth` on the admin endpoint, or a
config server `Policy` keyed off the `Identity` it returns.

## Install

```sh
go get github.com/xavidop/mamori/x/authjwt
```

## Usage

```go
package main

import (
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/x/authjwt"
)

type Config struct {
	Port int `mamori:"file:///etc/app.yaml#port"`
}

func run(ctx context.Context) error {
	auth, err := authjwt.New(authjwt.Config{
		Key:       authjwt.HMAC([]byte("your-signing-secret")),
		Issuer:    "https://issuer.example.com/",
		Audiences: []string{"mamori-admin"},
		Claims:    []string{"groups", "scope"},
	})
	if err != nil {
		return err
	}

	w, err := mamori.Watch[Config](ctx,
		mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
	)
	if err != nil {
		return err
	}
	defer w.Close()
	return nil
}
```

A valid request authenticates as `Identity{Subject: "<sub claim>", Attrs: {...}}`;
a missing, malformed, expired, or otherwise invalid token is denied with a
plain (401) error, never `mamori.ErrForbidden`.

## Key options

`Config.Key` supplies both the key material and the algorithms it is valid
for, so the two can never drift apart:

```go
func HMAC(secret []byte) KeyOption                  // HS256, HS384, HS512
func RSAPublicKey(key *rsa.PublicKey) KeyOption      // RS256/384/512, PS256/384/512
func ECDSAPublicKey(key *ecdsa.PublicKey) KeyOption  // ES256, ES384, ES512
func EdDSAPublicKey(key ed25519.PublicKey) KeyOption // EdDSA
```

For anything these don't cover, most commonly a JWKS endpoint with key
rotation, set `Config.Keyfunc` (a `jwt.Keyfunc`) directly - but then
`Config.Algorithms` **must** also be set explicitly. Leaving it empty is a
`Config` error, not a permissive default: an unrestricted algorithm set
reopens the alg-confusion vulnerability the `Key` helpers exist to close.

`Key` and `Keyfunc` are mutually exclusive; `New` returns an error if both,
or neither, are set.

## Config

```go
type Config struct {
	Key        KeyOption
	Keyfunc    jwt.Keyfunc
	Algorithms []string

	Issuer    string
	Audiences []string

	SubjectClaim string
	Claims       []string

	Realm string
}
```

- `Issuer`, if set, must match the token's `iss` claim exactly.
- `Audiences`, if non-empty, requires the token's `aud` claim to contain at
  least one listed value.
- `SubjectClaim` (default `"sub"`) names the claim copied into
  `Identity.Subject`.
- `Claims` lists additional claim names copied into `Identity.Attrs`: a
  string-valued claim becomes a single-element slice, a `[]string` (or JSON
  array of strings) becomes a multi-element slice, and `scope`/`scp` are
  special-cased even when string-valued - split on spaces into multiple
  values, matching how OAuth2/OIDC encode a space-delimited scope list as one
  string claim. A claim that's listed but absent from the token, or has an
  unrecognized shape, is simply absent from `Attrs`, never an error.
- `Realm`, if set, appears in the `WWW-Authenticate: Bearer realm="..."`
  challenge sent on a failed request.

## Security posture

`New` validates `Config` up front and returns an error for a nonsensical
configuration - a missing key source, or an unrestricted algorithm set -
rather than deferring that failure to the first request. On every request:

- **Algorithm allow-listing.** Parsing is restricted with
  `jwt.WithValidMethods` to exactly the algorithms implied by the configured
  key (or the explicit `Algorithms`). A token signed with `alg: none` is
  rejected outright, and so is a token signed with, say, HS256 when an RSA
  public key is configured - the classic RSA/HMAC key-confusion attack, where
  an attacker signs a forged token with HS256 using the server's own
  (public) RSA key as the HMAC secret. Each `Key` constructor's `Keyfunc`
  also double-checks the signing method's concrete type before returning key
  material, as defense in depth.
- **Expiration is mandatory.** `jwt.WithExpirationRequired` rejects both an
  expired token and a token with no `exp` claim at all.
- **Issuer and audience** are validated whenever configured; a mismatch is
  rejected.
- **Extraction is strict.** The token is read only from the `Authorization`
  header with a case-insensitive `Bearer ` scheme prefix; a missing header, a
  different scheme, or an empty token is rejected.
- **Failures never echo the token.** Errors name what failed (missing claim,
  invalid signature, wrong algorithm) but never the presented token or the
  full decoded claim set, so a misconfigured client can't leak either into
  logs or error-reporting pipelines.

A missing, malformed, expired, or otherwise invalid token is unauthenticated:
`Authenticate` returns a plain error, never `mamori.ErrForbidden`, so it
always produces a `401`. The returned authenticator implements `Challenger`,
sending `WWW-Authenticate: Bearer` (with a realm, if configured).

See the [auth docs](../../site/src/pages/docs/auth.md) for the full
`Authenticator`/`Challenger` picture and how the other shipped schemes
compose with this one (`AnyOf`, `AllOf`).

## Development

This module lives two levels below the repo root and uses a local `replace`
directive, so run every `go` command with the workspace disabled:

```sh
cd x/authjwt
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -race ./...
```
