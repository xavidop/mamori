---
layout: ../../../layouts/DocsLayout.astro
title: Auth
---

# Auth

An `Authenticator` decides whether an HTTP request may proceed and says who the caller is. One interface serves both mamori surfaces, the admin HTTP endpoint (`Handler`, `WithAdminHTTP`) and the [config server](/docs/server/), so a scheme configured for one works unchanged on the other.

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

## The Authenticator interface

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

`Subject` is a stable principal name; `Attrs` carries scheme-specific detail (certificate SANs, token claims, a peer uid/gid/pid) and is multi-valued so a scheme can return groups, scopes, or multiple SANs directly. The admin endpoint ignores the `Identity` (it only serves metadata); the [config server](/docs/server/) consumes it, since its `Policy` decides what a caller may see based on it.

Two optional pieces complete the interface:

- `Challenger` supplies the `WWW-Authenticate` header on a `401`; a scheme that does not implement it produces a bare `401`.
- `ErrForbidden` selects the status code: return it from `Authenticate` to produce a `403` (authenticated but not permitted) rather than a `401`.

```go
type Challenger interface {
	Challenge() string
}

var ErrForbidden = errors.New("mamori: forbidden")
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

Applying `WithAuth` more than once panics rather than silently letting the second call win; compose multiple schemes explicitly with `AnyOf` or `AllOf` instead.

## Next

- [Auth schemes](/docs/auth/schemes/): BasicAuth, BearerToken, APIKey, MTLS, PeerCred, JWT, AnyOf/AllOf.
- [Custom authenticators](/docs/auth/custom/): the `Func` variants, credential rotation, and writing your own.
- [HTTP exposure](/docs/observability/admin/) covers the admin endpoint the auth attaches to.
- [Config server](/docs/server/) is the second surface, where `Policy` authorizes against the `Identity`.
