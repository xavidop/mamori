---
layout: ../../../layouts/DocsLayout.astro
title: Custom authenticators
---

# Custom authenticators

Rotate a shipped scheme's credential live with the `Func` variants, or implement the `Authenticator` interface yourself. For the interface and the shipped schemes, see the [overview](/docs/auth/) and [Auth schemes](/docs/auth/schemes/).

## The Func variants and credential rotation

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

When the secret manager rotates `prod/admin#token`, `adminW` picks it up on its own reconcile loop (see [Loading and watching](/docs/usage/)), and the very next request is checked against the new value: no restart, and no gap where the endpoint accepts no credential. Until the first successful resolve, `AdminToken` is a zero `secret.String`, and the fail-closed rule below denies every request.

## Constant-time comparison and fail-closed behavior

`BasicAuth`, `BearerToken`, and `APIKey` all compare credential material with `crypto/subtle.ConstantTimeCompare`, never `==` or `bytes.Equal`. A naive comparison short-circuits on the first mismatching byte, which would let an attacker recover a valid credential one byte at a time by timing requests; constant-time comparison always walks the full length of both operands. (`MTLS`'s CommonName/DNS-SAN check is an ordinary string comparison, since those are public identifiers on an already-verified certificate, not secrets.)

All three secret-based schemes also fail closed on an unconfigured credential: if the expected value is a zero `secret.String` (`IsZero()` is true), every request is denied outright, before even looking at what the caller presented. "Unset" is never treated as "no credential required".

None of the failure messages echo the value the caller presented, only that the credential was missing or invalid, so a misconfigured client cannot leak its guess into logs.

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

Implement `Authenticator` (and optionally `Challenger`) on a named type when the check needs its own state or its own `WWW-Authenticate` value. Whichever shape you pick, keep the two properties the shipped schemes share: compare any caller-supplied secret material with `crypto/subtle.ConstantTimeCompare` rather than `==`, and fail closed (deny the request outright) whenever the expected credential is unset, rather than treating "not configured" as "no credential required".
