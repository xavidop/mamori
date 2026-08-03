---
layout: ../../../layouts/DocsLayout.astro
title: Generic HTTPS
---

# Generic HTTPS

Load configuration and secrets from an HTTP endpoint you declare - your own REST API, not a named vendor. Built on `providers/httpcore`; no other provider in this repo names no vendor the way this one does.

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

**This is an ordinary import, never a blank one, and this provider has no `init()`.** Every other provider registers itself from `init`, so `import _` is enough, because each has a vendor default: `New()` is valid with no arguments and reads credentials from the environment at resolve time. This one has no vendor and therefore no default - `Name`, `BaseURL`, `Auth`, `Query` and `Header` are all operator-supplied, `New` requires at least one `Endpoint`, and `New` returns an error, none of which an `init` function has anything to pass or anywhere to report. Registering an endpointless provider globally would be worse than not registering one: it would advertise `https://` in `RegisteredSchemes()` and to `mamori doctor` while failing every ref.

Hand the constructed provider to `mamori.WithProvider`, which takes precedence over the global registry for its scheme, or to `mamori.Register` once you have built it.

## Using the ref

An `https://` ref points at a path on one registered `Endpoint`, optionally selecting a field from a JSON value stored there.

```text
https://<endpoint>/<path>
https://<endpoint>/<path>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<endpoint>` | yes | Not a hostname - the `Name` of an `Endpoint` registered with `New`. A ref naming an unregistered endpoint fails with `mamori.ErrInvalid`, caught by `mamori doctor` before deployment. |
| `<path>` | yes | Everything after the endpoint name; joined onto that endpoint's `BaseURL`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued response via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `https://billing/cfg` reads path `cfg` on the endpoint named `billing`.
- `https://billing/cfg/main` reads path `cfg/main` on `billing` - only the first ref-path segment names the endpoint.
- `https://billing/cfg#level` reads top-level field `level` of the JSON document at `cfg`.
- `https://billing/cfg#/db/pass` reads a nested field of `cfg`, selected by JSON Pointer.
- `https://billing/${TENANT}/cfg` interpolates `TENANT` from `WithRefVars` at load time.

```go
type Config struct {
	LogLevel string `source:"https://billing/cfg#level"`
	DBPass   string `source:"https://billing/cfg#/db/pass"`
}
```

## Ref paths are percent-escaped

A ref path is a percent-escaped path: `%2F` addresses a single path segment whose own name contains a slash, and a literal percent sign must be written `%25`, never a bare `%`, or `Resolve` rejects the ref before any request reaches the backend.

```
https://billing/discount-50%-off        # rejected: bare "%" is not a valid escape
https://billing/discount-50%25-off      # reaches the backend as discount-50%-off
```

## Why endpoints are named, not raw URLs

A ref cannot carry target query parameters: mamori's grammar puts the fragment before the query (`path#key?opts`), the reverse of a standard URL's `path?query#fragment`, so a ref written the standard way, `https://api.example.com/cfg?env=prod#/db/pass`, does not fail loudly - `ParseRef` splits off everything from the first `?` onward as `Opts` before it ever looks for a `#`, so `Key` comes out empty and `Opts` comes out as `env` = `"prod#/db/pass"`. Fixed query parameters live on the `Endpoint`'s `Query` field instead.

A ref cannot carry credentials either, because a struct tag is source code; authentication lives on the `Endpoint`'s `Auth` field. And a provider that fetched an arbitrary URL named by a struct tag would make every `source` tag a potential SSRF vector. Restricting refs to declared endpoints matches the posture the rest of mamori takes: the config server serves a fixed, operator-declared binding table and never a client-supplied ref.

## Ref paths cannot escape their endpoint

A ref path containing a `.` or `..` segment is rejected with `mamori.ErrInvalid`, before it is joined onto the endpoint's `BaseURL` and before any request goes out. This is not theoretical: `${VAR}` interpolation from `WithRefVars` means a ref like `https://billing/${TENANT}/cfg` carries whatever the application put in `TENANT` at runtime. Without this check, a `TENANT` of `../..` would reach another tenant's configuration without ever leaving the declared host, so the endpoint restriction above would never fire.

**The check lives in `httpcore`, not here**, so that every provider built on it inherits the same guarantee and none can forget it. That is a claim about where the check lives, not that `https://`'s ref behaviour is unchanged in every respect: elsewhere on this page, a percent-encoded traversal (`%2e%2e`) now fails where it used to resolve, and a literal `%` now must be written `%25`.

The check splits on **both** `/` and `\`, not `/` alone: splitting only on `/` would leave `a\..\..\secrets` as one segment matching neither `.` nor `..`, and the request would go out with its backslashes percent-encoded as `%5C` - which IIS and ASP.NET decode and honor as a directory separator, the classic backslash traversal bypass. A backslash inside an ordinary key still works; only a traversal is refused. Three-dot segments are not matched: RFC 3986 defines dot-segment removal over exactly `.` and `..`, so `...` is an ordinary name.

It runs on the **decoded** path, so `%2e%2e` is refused exactly like `..`. `httpcore` preserves a caller's percent escapes to the wire, because a key whose own name contains a slash cannot be addressed otherwise, and an encoded traversal would be preserved with them.

It rejects rather than cleans, deliberately: cleaning a path would silently change which value a ref names, and `mamori doctor` resolving every ref before deployment means a rejected ref surfaces there, not in production.

```go
ref, _ := mamori.ParseRef("https://billing/../secrets")
_, err := p.Resolve(context.Background(), ref)
fmt.Println(errors.Is(err, mamori.ErrInvalid))
// Output: true
```

## Endpoint options

| Field | Default | Effect |
| --- | --- | --- |
| `Name` | required | The ref authority, e.g. `billing`. Must be non-empty and must not contain `/`. |
| `BaseURL` | required | The root every ref path is joined onto. Scheme must be `http` or `https`; `http://` is rejected unless `AllowInsecure` is set. |
| `Auth` | `nil` (unauthenticated) | An `httpcore.Authenticator` - `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth`, or `OAuth2ClientCredentials`. |
| `Query` | `nil` | Merged into every request to this endpoint. |
| `Header` | `nil` | Merged into every request to this endpoint. |
| `Client` | `nil` (httpcore's default) | The `*http.Client` performing round trips. |
| `MaxBody` | `0` (`httpcore.DefaultMaxBody`, 1 MiB) | Caps the response size read from this endpoint. |
| `Sensitive` | `false` | Marks every `Value` from this endpoint as secret. |
| `AllowInsecure` | `false` | Permits an `http://` `BaseURL` - cleartext `http` and nothing else, never a way to skip the scheme check itself. |

```go
import (
	"github.com/xavidop/mamori/providers/httpcore"
	httpsprov "github.com/xavidop/mamori/providers/https"
)

p, err := httpsprov.New(httpsprov.Endpoint{
	Name:    "billing",
	BaseURL: "https://billing.internal.example.com/v1",
	Auth:    httpcore.Bearer(os.Getenv("BILLING_API_TOKEN")),
})
if err != nil {
	return err
}

cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

`New` validates every endpoint at construction: it fails on a missing/duplicate/slashed `Name`, a missing or unparsable `BaseURL`, or a `BaseURL` scheme outside the closed `http`/`https` set - checked as a closed set rather than merely testing for `http://`, so an `ftp://` typo fails at startup instead of on every resolve with `net/http`'s "unsupported protocol scheme".

## Error classification

`Resolve` classifies every non-2xx, non-304 response through `httpcore.ClassifyStatus`, unmodified:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

422 is named rather than left to the default because the default kind is transient: mamori would back off and retry a request that was well formed and semantically wrong, which retrying can never fix.

The response body never reaches an error. `ClassifyStatus` takes only a caller-supplied `detail` string, `httpcore.Config.ErrorDetail` is the hook that supplies one, and this provider leaves it nil, because the body it just fetched can itself be the config value or secret a ref is resolving.

## Watch

mamori polls this provider - a generic, operator-declared endpoint exposes no push channel to subscribe to. Each `Endpoint` is wired to its own `httpcore.Revalidator`, keyed by the ref's raw string, so every poll after the first sends `If-None-Match` / `If-Modified-Since` and an unchanged value costs a `304` with an empty body rather than the full payload again. `Value.Version` comes from `httpcore.Version`, derived from whichever validator the response actually carried. This provider does not implement `WatchableProvider`; compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads beyond what conditional GET already saves.

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

There is no zero-config default: at least one `Endpoint` is required, each naming a `BaseURL` whose scheme is `http` or `https` (`http://` only with `AllowInsecure`). `Query` and `Header` cover a fixed target query string or header, since a ref itself cannot carry either.

Verified with an in-process fake `http.RoundTripper` (endpoint validation, ref resolution and selection, conditional GET, dot-segment rejection, error classification), so the conformance kit runs without a live backend. A `//go:build integration` test exercises a real HTTP endpoint when `MAMORI_HTTPS_BASE_URL` and `MAMORI_HTTPS_PATH` are set.
