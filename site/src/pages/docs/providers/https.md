---
layout: ../../../layouts/DocsLayout.astro
title: Generic HTTPS
---

# Generic HTTPS

Load configuration and secrets from an HTTP endpoint you declare: your own REST API, rather than a named vendor. Reach for it when your config lives behind a service your team owns.

| | |
| --- | --- |
| Scheme | `https://` |
| Module | `github.com/xavidop/mamori/providers/https` |
| Sensitive | per-endpoint (`Endpoint.Sensitive`) |
| Watch | poll (conditional GET) |
| Auth | per-endpoint (`Endpoint.Auth`), none by default |

## Install

```bash
go get github.com/xavidop/mamori/providers/https
```

```go
import (
	"github.com/xavidop/mamori"
	httpsprov "github.com/xavidop/mamori/providers/https"
)

p, err := httpsprov.New(httpsprov.Endpoint{
	Name:    "billing",
	BaseURL: "https://billing.internal.example.com/v1",
})
if err != nil {
	return err
}

cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Unlike other providers, this one takes an ordinary import rather than a blank one, and you pass the result to `mamori.WithProvider` or `mamori.Register`. There is no vendor default to fall back on, so at least one endpoint is always required.

## Using the ref

An `https://` ref points at a path on one registered endpoint, optionally selecting a field from a JSON value stored there.

```text
https://<endpoint>/<path>
https://<endpoint>/<path>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<endpoint>` | yes | The `Name` of an endpoint you registered, **not a hostname**. An unregistered name fails with `mamori.ErrInvalid`. |
| `<path>` | yes | Everything after the endpoint name, joined onto that endpoint's `BaseURL`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON response: a top-level key, or an RFC 6901 JSON Pointer for a nested one. |

**Examples**

- `https://billing/cfg` reads path `cfg` on the endpoint named `billing`.
- `https://billing/cfg/main` reads path `cfg/main`; only the first segment names the endpoint.
- `https://billing/cfg#level` reads top-level field `level` of the JSON at `cfg`.
- `https://billing/cfg#/db/pass` reads a nested field, selected by JSON Pointer.
- `https://billing/${TENANT}/cfg` interpolates `TENANT` from `WithRefVars` at load time.

```go
type Config struct {
	LogLevel string `source:"https://billing/cfg#level"`
	DBPass   string `source:"https://billing/cfg#/db/pass"`
}
```

## Declaring endpoints

The endpoint name in a ref is one you choose. Credentials and any fixed query string or headers live here rather than in the ref, because a struct tag is source code and cannot safely carry a token.

| Field | Default | Effect |
| --- | --- | --- |
| `Name` | required | The name refs use, e.g. `billing`. No `/`, `?` or `#`. |
| `BaseURL` | required | The root every ref path is joined onto. Must be `https://`, or `http://` with `AllowInsecure`. |
| `Auth` | none | An `httpcore.Authenticator`: `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth`, or `OAuth2ClientCredentials`. |
| `Query` | none | Query parameters added to every request to this endpoint. |
| `Header` | none | Headers added to every request to this endpoint. |
| `Client` | default | The `*http.Client` used for requests. |
| `MaxBody` | 1 MiB | Caps the response size read from this endpoint. |
| `Sensitive` | `false` | Marks every value from this endpoint as a secret, so it is redacted in logs. |
| `AllowInsecure` | `false` | Permits a cleartext `http://` `BaseURL`. Nothing else. |

```go
p, err := httpsprov.New(httpsprov.Endpoint{
	Name:      "billing",
	BaseURL:   "https://billing.internal.example.com/v1",
	Auth:      httpcore.Bearer(os.Getenv("BILLING_API_TOKEN")),
	Sensitive: true,
})
```

Endpoints are validated when you construct the provider, so a typo in a name or a `BaseURL` fails at startup rather than on every resolve.

## Path rules

Two rules apply to the `<path>` part of a ref.

**Write a literal `%` as `%25`.** A ref path is percent-escaped, so `%2F` addresses a single path segment whose own name contains a slash.

```text
https://billing/discount-50%-off      rejected
https://billing/discount-50%25-off    reaches the backend as discount-50%-off
```

**A path cannot climb out of its endpoint.** A `.` or `..` segment is rejected with `mamori.ErrInvalid` before any request is sent. This matters when a path is interpolated: in `https://billing/${TENANT}/cfg`, a `TENANT` of `../..` would otherwise reach a different tenant's configuration. Both the plain and percent-encoded forms are refused, and `mamori doctor` surfaces a bad ref before you deploy.

## Error classification

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

A `404` is reported as `not_found`, so a field's `default:` or `optional` handling applies as usual.

Response bodies never appear in an error message, since the body being fetched may itself be the secret.

## Watch

mamori polls this provider; a generic endpoint has no push channel to subscribe to. Polls after the first send `If-None-Match` and `If-Modified-Since`, so an unchanged value costs a `304` with an empty body rather than the whole payload again. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads further.

## Configuration

```go
import (
	"github.com/xavidop/mamori/providers/httpcore"
	httpsprov "github.com/xavidop/mamori/providers/https"
)

p, err := httpsprov.New(httpsprov.Endpoint{
	Name:      "billing",
	BaseURL:   "https://billing.internal.example.com/v1",
	Auth:      httpcore.Bearer(os.Getenv("BILLING_API_TOKEN")),
	Sensitive: true,
})
if err != nil {
	return err
}

opt := mamori.WithProvider(p)
```

Tested against an in-process HTTP fake, so the conformance suite runs without a live backend. A `//go:build integration` test exercises a real endpoint when `MAMORI_HTTPS_BASE_URL` and `MAMORI_HTTPS_PATH` are set.
