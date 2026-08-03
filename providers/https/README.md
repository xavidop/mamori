# mamori - generic HTTPS provider

A [mamori](https://github.com/xavidop/mamori) provider for configuration and
secrets served over HTTP by an endpoint you declare, built on
[`providers/httpcore`](../httpcore/). Unlike every other provider in this repo,
it names no vendor: it is what you reach for when your configuration or
secrets live behind a REST API of your own, one your team owns rather than one
mamori already speaks to.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

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

**This is an ordinary import, never a blank one, and there is no `init()`.**
Every other provider in this repo registers itself from `init` so that
`import _` is enough, because each has a vendor default: `New()` is valid with
no arguments and reads its credentials from the environment at resolve time.
This provider has no vendor and therefore no default. An `Endpoint`'s `Name`,
`BaseURL`, `Auth`, `Query` and `Header` are all operator-supplied, `New`
requires at least one endpoint, and `New` returns an error - none of which an
`init` function has anything to pass or anywhere to report. Registering an
endpointless provider globally would be worse than not registering one: it
would advertise `https://` in `mamori.RegisteredSchemes()` and to
`mamori doctor` while failing every ref, which is exactly the resolve-time
failure `New` exists to turn into a startup-time one.

Hand the constructed provider to `mamori.WithProvider`, which takes precedence
over the global registry for its scheme, or to `mamori.Register` if you would
rather register it globally once you have built it.

## Scheme

```
https://<endpoint>/<path>[#<key>][?<opts>]
```

`<endpoint>` is not a hostname. It is the `Name` of an `Endpoint` you
registered with `New`, and a ref naming an unregistered endpoint fails with
`mamori.ErrInvalid` so `mamori doctor` catches the typo before deployment. The
actual network destination, `Endpoint.BaseURL`, never appears in a ref at all.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `https://billing/cfg` | Path `cfg` on the endpoint named `billing` |
| `https://billing/cfg/main` | Path `cfg/main` on `billing`; only the first ref-path segment names the endpoint |
| `https://billing/cfg#level` | Top-level field `level` of the JSON document at `cfg` |
| `https://billing/cfg#/db/pass` | A nested field of `cfg`, selected by RFC 6901 JSON Pointer |
| `https://billing/${TENANT}/cfg` | Path interpolated from `WithRefVars`'s `TENANT` at load time |

```go
type Config struct {
    LogLevel string `source:"https://billing/cfg#level"`
    DBPass   string `source:"https://billing/cfg#/db/pass"`
}
```

### Ref paths are percent-escaped

A ref path is a percent-escaped path, the same contract
[`providers/httpcore`](../httpcore/)'s `Request.Path` documents: `%2F`
addresses a single path segment whose own name contains a slash, and a literal
percent sign must be written `%25`, never a bare `%`, or `Resolve` rejects the
ref before any request reaches the backend.

```
https://billing/discount-50%-off        # rejected: bare "%" is not a valid escape
https://billing/discount-50%25-off      # reaches the backend as discount-50%-off
```

## Why endpoints are named, not raw URLs

A ref cannot carry target query parameters. mamori's grammar is
`scheme://path[#key][?opts]` with the fragment BEFORE the query - the reverse
of a standard URL's `path?query#fragment` - and `?opts` is mamori's own
namespace for `?decode=` and `?debounce=`, not a place for this provider to
put a target's fixed query string. A ref written that follows the standard
URL convention instead of mamori's, `https://api.example.com/cfg?env=prod#/db/pass`,
does not fail loudly: `ParseRef` splits off everything from the first `?`
onward as `Opts` before it ever looks for a `#`, so `Key` comes out empty and
`Opts` comes out as `env` = `"prod#/db/pass"` - the intended field selection
silently vanishes into a query value instead of erroring. Fixed query
parameters therefore live on the `Endpoint`, in its `Query` field, not in the
ref.

```go
ref, _ := mamori.ParseRef("https://api.example.com/cfg?env=prod#/db/pass")
fmt.Println(ref.Key == "", ref.Opts.Get("env"))
// Output: true prod#/db/pass
```

A ref cannot carry credentials, because a struct tag is source code, and
source code ends up in version control, logs, and shoulder surfing.
Authentication lives on the `Endpoint`'s `Auth` field, constructed once at
startup from wherever your process actually keeps secrets.

A provider that fetched an arbitrary URL named by a struct tag would make
every `source` tag a potential SSRF vector: a field's tag is often built or
edited by whoever writes the struct, not whoever reviews outbound network
access. Restricting refs to declared endpoints matches the posture the rest of
mamori takes - the config server serves a fixed, operator-declared binding
table and never a client-supplied ref, and `exec:` is opt-in for the same
class of reason.

## Ref paths cannot escape their endpoint

A ref path containing a `.` or `..` segment is rejected, wrapping
`mamori.ErrInvalid`, rather than joined onto the endpoint's `BaseURL` and shown
to the backend. The rejection happens before the round trip, so the backend
never sees the path at all.

**That check lives in [`providers/httpcore`](../httpcore/), not here.** Every
provider built on `httpcore` inherits it, so none can forget it, which matters
because the module exists to be built on: a check each provider has to remember
is a check the next provider ships without. That is a claim about *where* the
check lives, not that `https://`'s ref behaviour is unchanged in every respect:
elsewhere in this document, a percent-encoded traversal (`%2e%2e`) now fails
where it used to resolve, and a literal `%` now must be written `%25`.

This is not theoretical. `expandRefVars` substitutes `${VAR}` from
`WithRefVars` at runtime, so a ref of `https://billing/${TENANT}/cfg` carries
whatever the application put in `TENANT`. Without this check, a `TENANT` of
`../..` would reach outside the path prefix `billing`'s `BaseURL` declares -
for an endpoint scoped to `https://api.example.com/v1/tenants/acme`, that is
another tenant's configuration, reached without ever leaving the declared
host, so the endpoint restriction the section above describes never fires.

The check splits on **both** `/` and `\`, not `/` alone. Splitting only on `/`
would leave `a\..\..\secrets` as a single segment matching neither `.` nor
`..`, so the check would pass and the request would go out with its
backslashes percent-encoded as `%5C` on the wire. Most backends treat that as
an ordinary character, but IIS and ASP.NET decode it and honor `\` as a
directory separator - the classic backslash traversal bypass. `BaseURL` is
operator-supplied with no platform restriction, so this package cannot assume
the backend is never one of them. Splitting on both keeps `a\b` usable as an
ordinary key while still refusing `a\..\b`.

It rejects rather than cleans, deliberately. Cleaning a path (resolving `..`
away) would silently change which value a ref names, and a ref that quietly
means something other than what it says is worse than one that fails loudly.
`mamori doctor` resolves every ref before deployment, so a rejected ref
surfaces there, not in production.

```go
p, err := httpsprov.New(httpsprov.Endpoint{
    Name:    "billing",
    BaseURL: "https://billing.internal.example.com/v1",
})
if err != nil {
    panic(err)
}

for _, raw := range []string{
    "https://billing/../secrets",
    `https://billing/a\..\..\secrets`,
} {
    ref, _ := mamori.ParseRef(raw)
    _, err := p.Resolve(context.Background(), ref)
    fmt.Println(errors.Is(err, mamori.ErrInvalid))
}
// Output:
// true
// true
```

A backslash inside an ordinary key still works; only a traversal is refused.
Segments of three or more dots are not matched either: RFC 3986 section 5.2.4
defines dot-segment removal over exactly `.` and `..`, so `...` is an ordinary
segment name.

A percent-encoded traversal (`%2e%2e`) is refused exactly like a literal one,
because the check runs on the **decoded** path. It used to need no check of its
own: writing a path into `url.URL.Path` alone re-encoded the `%` sign, so the
backend received the harmless literal `%252e%252e`. `httpcore` now preserves a
caller's escapes all the way to the wire, since a backend whose key names may
contain slashes cannot be addressed without an encoded slash surviving, and an
encoded traversal would have survived with it.

One case falls out of that: a key literally named `a/../b`, addressed with the
encoded slashes this escaping exists to support (`a%2F..%2Fb`), is still
rejected as a traversal, because the check runs on the path after those
escapes are decoded - deliberate over-rejection on the safe side, not a bug to
route around.

## Endpoint options

| Field | Default | Effect |
| --- | --- | --- |
| `Name` | none (required) | The ref authority, e.g. `billing` in `https://billing/cfg`. Must be non-empty and must not contain `/`. |
| `BaseURL` | none (required) | The root every ref path is joined onto. Must parse as a URL whose scheme is `http` or `https`; `http://` is rejected unless `AllowInsecure` is set. |
| `Auth` | `nil` (unauthenticated) | Injects credentials via an `httpcore.Authenticator` - `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth`, or `OAuth2ClientCredentials`. |
| `Query` | `nil` | Merged into every request to this endpoint. Exists because a ref cannot carry target query parameters; see above. |
| `Header` | `nil` | Merged into every request to this endpoint. |
| `Client` | `nil` (httpcore's default: 30s timeout) | The `*http.Client` performing round trips. |
| `MaxBody` | `0` (selects `httpcore.DefaultMaxBody`, 1 MiB) | Caps the response size read from this endpoint. |
| `Sensitive` | `false` | Marks every `Value` from this endpoint as secret, driving redaction downstream. Per-endpoint, because a generic HTTP endpoint may serve either secrets or plain configuration and mamori cannot infer which. |
| `AllowInsecure` | `false` | Permits an `http://` `BaseURL`. Fetching configuration in cleartext exposes it to anything on the path, so it must be opted into; it permits cleartext `http` and nothing else - it is not a general "skip the scheme check" switch. |

```go
p, err := httpsprov.New(httpsprov.Endpoint{
    Name:          "billing",
    BaseURL:       "https://billing.internal.example.com/v1",
    Auth:          httpcore.Bearer(os.Getenv("BILLING_API_TOKEN")),
    Query:         url.Values{"env": {"prod"}},
    Header:        http.Header{"X-Tenant": {"acme"}},
    MaxBody:       2 << 20,
    Sensitive:     true,
    AllowInsecure: false,
})
if err != nil {
    panic(err)
}

cfg, err := mamori.Load[Config](context.Background(), mamori.WithProvider(p))
```

`New` validates every endpoint at construction, not at the first resolve. It
fails when no endpoint is supplied, when a `Name` is empty, duplicated, or
contains `/`, when a `BaseURL` is missing, unparsable, or has a scheme other
than `http`/`https`, or when a `http://` `BaseURL` is given without
`AllowInsecure`. The scheme is checked against that closed set rather than
merely testing for `http://`: an `ftp://` typo or a `ws://` paste would
otherwise construct cleanly and then fail on every resolve with `net/http`'s
"unsupported protocol scheme" - a resolve-time failure `New` exists precisely
to turn into a startup-time one.

## Error classification

`Resolve` classifies every non-2xx, non-304 response through
`httpcore.ClassifyStatus`, unmodified:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

422 Unprocessable Entity is named rather than left to fall through to the
default, because the default kind is transient: mamori would back off and retry
a request that was well formed and semantically wrong, and no number of
retries can fix that.

The response body never reaches an error. `ClassifyStatus` takes only a
caller-supplied `detail` string, `httpcore.Config.ErrorDetail` is the hook that
supplies one, and this provider leaves that hook nil, because the body it just
fetched can itself be the config value or secret a ref is resolving. A `403` on
a ref pointing at a secret reports `permission_denied` with no part of that
secret anywhere in the error text.

## Conditional GET

mamori polls this provider - see "No native watch" below - so the same ref is
fetched again on every tick. Each `Endpoint` is wired to its own
`httpcore.Revalidator` (`httpcore.NewRevalidator(client, 0)`, the default 512
entries), keyed by the ref's raw string, so two fields reading the same ref
share one cache entry. Every poll after the first sends `If-None-Match` /
`If-Modified-Since`, and an unchanged value costs a `304` with an empty body
rather than the full payload again. `Value.Version` comes from
`httpcore.Version`, which derives it from whichever validator the response
actually carried, falling back to a body hash when the backend sends neither.

## No native watch

A generic, operator-declared HTTP endpoint exposes no push channel this
provider could subscribe to - there is no vendor SDK here to define one, only
whatever REST API you pointed `BaseURL` at. This provider deliberately does
not implement `mamori.WatchableProvider`, and mamori wraps it in the polling
adapter instead. `TestProviderIsNotWatchable` pins that absence as a decision,
not an oversight: it fails if a future change makes `Provider` satisfy
`WatchableProvider`, so that cannot happen silently.

If many refs share a poll interval and you want to avoid a full round trip on
every tick beyond what conditional GET already saves, compose
[`middleware.Cache`](../../middleware/) in front of this provider.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake HTTP transport (`go test ./...`) |
| Wire behaviour (request shape, conditional-GET headers, error responses) | **Verified** - this provider defines its own contract; there is no vendor API to confirm against |
| `New` validation: no endpoints, empty/duplicate/slashed `Name`, unparsable `BaseURL` | **Verified** (`TestNewRejectsNoEndpoints`, `TestNewRejectsUnnamedEndpoint`, `TestNewRejectsDuplicateNames`, `TestNewRejectsNameWithSlash`, `TestNewRejectsUnparsableBaseURL`) |
| `BaseURL` scheme is a closed set (`http`/`https` only), not merely an `http://` test; `AllowInsecure` does not rescue a non-http scheme | **Verified** (`TestNewRejectsNonHTTPScheme`, `TestAllowInsecureDoesNotRescueOtherSchemes`) |
| An empty `BaseURL` fails with this provider's own message; a `http://` `BaseURL` is rejected unless `AllowInsecure` | **Verified** (`TestNewRejectsEmptyBaseURL`, `TestNewRejectsInsecureBaseURL`, `TestNewAllowsInsecureWhenOptedIn`) |
| `Resolve` returns the raw body; `#field` and `#/json/pointer` selection | **Verified** (`TestResolveReturnsBody`, `TestResolveSelectsWithPointer`, `TestResolveSelectsTopLevelKey`) |
| Unknown endpoint reports `mamori.ErrInvalid`, never `mamori.ErrNotFound` (which would silently apply a field's default) | **Verified** (`TestResolveUnknownEndpoint`) |
| An absent value reports `mamori.ErrNotFound` | **Verified** (`TestResolveMissingValueIsNotFound`) |
| `Endpoint.Query` and `Endpoint.Header` are merged into every request | **Verified** (`TestResolveMergesEndpointQueryAndHeader`) |
| `Endpoint.Sensitive` propagates to `Value.Sensitive` | **Verified** (`TestResolveMarksSensitive`) |
| The second `Resolve` of the same ref sends a conditional GET and reuses the cached value on a 304 | **Verified** (`TestResolveUsesConditionalGetOnSecondCall`) |
| A ref path with a `.`/`..` segment, including a backslash-separated one, is rejected as `mamori.ErrInvalid` | **Verified** (`TestResolveRejectsDotSegments`, plus `httpcore`'s `TestDoRejectsDotSegments`) |
| A backslash inside an ordinary key still resolves; only a traversal is refused | **Verified** (`TestResolveAllowsBackslashInAnOrdinaryKey`) |
| A percent-encoded traversal (`%2e%2e`) is rejected too, and never reaches the backend | **Verified** (`TestResolveRejectsEscapedDotSegments`) |
| An unrecognized ref option (`?decode=`) passes through untouched; decoding is core's job | **Verified** (`TestResolvePassesThroughUnknownOptions`) |
| Every classified HTTP status (400/401/403/404/408/429/5xx) maps to its documented `mamori.Kind` | **Verified** (`TestStatusToKind`, plus `providertest`'s `ErrorClassification` case) |
| A failing response's body never reaches the returned error | **Verified** (`TestResolveErrorCarriesNoPayload`) |
| `WatchableProvider` is deliberately not implemented | **Verified** (`TestProviderIsNotWatchable`) |
| End-to-end resolution and conditional-GET revalidation against a real HTTP endpoint | **Requires a live endpoint, not run in CI** - see the integration test below |

The unit and conformance tests run against an in-process fake `http.RoundTripper`
(`go test ./...`), so they require no network access and no real backend.

### Live integration test

An integration test exercises a real HTTP endpoint you nominate. It is guarded
by a build tag and skips unless `MAMORI_HTTPS_BASE_URL` and
`MAMORI_HTTPS_PATH` are both set; nothing it does ever logs a token or a
resolved value, only a byte count:

```sh
export MAMORI_HTTPS_BASE_URL=https://api.example.com/v1
export MAMORI_HTTPS_PATH=config
export MAMORI_HTTPS_TOKEN=...          # optional bearer token
export MAMORI_HTTPS_POINTER=/db/host   # optional JSON Pointer
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace
disabled:

```sh
cd providers/https
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
