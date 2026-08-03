# mamori - httpcore

The shared HTTP resolve core for mamori providers whose backend is a REST API.
Before this package existed, sixteen of mamori's providers each hand-rolled
request building, credential injection, status classification, and response
body hygiene, and issue #107 was that duplication surfacing as a bug:
inconsistent body draining across providers left some connections unreused.
`httpcore` exists so a provider author writes the part that is actually
specific to their backend - the URL shape, the response envelope - and
inherits request building, authentication, classification, and body hygiene
from one place instead of copying it.

## Install

```sh
go get github.com/xavidop/mamori/providers/httpcore
```

`httpcore` is a library, not a provider: it registers no scheme, so importing
it is an ordinary import, never a blank one.

```go
import "github.com/xavidop/mamori/providers/httpcore"
```

## Client

`New` builds a `Client` from a `Config`: a `BaseURL` (required), an optional
`Authenticator`, an optional `*http.Client`, and a response size ceiling
(`MaxBody`, defaulting to `DefaultMaxBody`, 1 MiB). `Do` performs one round
trip against it: it applies `Auth`, joins `Request.Path` onto `BaseURL`,
bounds and always drains the response body, and classifies a non-2xx status
through `ClassifyStatus`.

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte(`{"level":"debug"}`))
}))
defer srv.Close()

c, err := httpcore.New(httpcore.Config{
    BaseURL: srv.URL,
    Auth:    httpcore.Bearer("token"),
})
if err != nil {
    panic(err)
}

resp, err := c.Do(context.Background(), httpcore.Request{Path: "/config"})
if err != nil {
    panic(err)
}
fmt.Println(string(resp.Body))
// Output: {"level":"debug"}
```

`Do` can fail for several distinct reasons: request construction (a
malformed method or path, or an `Authenticator` whose `Apply` itself errors),
a transport failure (the request never got a response), a non-2xx/non-304
status (classified through `ClassifyStatus`), or a response over `MaxBody`.
The transport case gets special handling worth knowing about: `net/http`'s
`Client.Do` wraps a transport failure - a refused connection, a timeout, a
cancelled context - in a
`*url.Error` whose `Error()` renders the request URL via `stripPassword`,
which masks only a userinfo password in that URL and leaves any query string
untouched. A credential injected by `QueryAuth` therefore survives in that
rendered URL and would otherwise leak into `err.Error()`. `Do` rebuilds that
`*url.Error` with a redacted URL rather than discarding it, so `errors.As`
still reaches `*url.Error` and `errors.Is` still reaches the underlying cause
(`context.Canceled`, a `*net.OpError`, ...); only the rendered text changes.

## Authenticators

| Authenticator | Use it when |
| --- | --- |
| `Bearer(token)` | The backend takes an RFC 6750 bearer token in `Authorization`, which is what most REST config and secret backends want. |
| `HeaderAuth(name, value)` | The backend wants a named API-key header instead, such as `X-Api-Key`. |
| `BasicAuth(user, pass)` | The backend requires or prefers HTTP Basic credentials. |
| `QueryAuth(name, value)` | The backend accepts no header form, only a query parameter. |
| `OAuth2ClientCredentials(cfg)` | The backend is an OAuth2 identity provider using the RFC 6749 client-credentials grant; the returned authenticator fetches, caches, and refreshes the access token. |

`QueryAuth` puts the credential in the request line itself, where a proxy's
access log or a server's own request log can see it in plain text; prefer any
of the header-based authenticators wherever the backend allows one.

**Two types share the name `Authenticator`, with opposite directions of
travel.** `mamori.Authenticator` (the root module's `auth.go`) authenticates
*inbound* requests arriving at mamori's admin server and returns an
`Identity`. `httpcore.Authenticator`, defined here, injects credentials into
*outbound* requests this package sends to a provider's own backend. There is
no compile conflict, since nothing dot-imports either package, but a provider
author who imports both `mamori` and `httpcore` in one file is looking at two
`Authenticator` types that mean opposite things; the package qualifier is
what tells them apart.

`OAuth2ClientCredentials` returns `(Authenticator, error)`, not a bare
`Authenticator`: `TokenURL`, `ClientID`, and `ClientSecret` are validated at
construction, so a missing one surfaces immediately rather than as an opaque
failure on the first `Apply`. The token itself is still fetched lazily, on
the first `Apply`, so building a provider never blocks on the network.

```go
tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"access_token":"abc123","expires_in":3600}`))
}))
defer tokenSrv.Close()

auth, err := httpcore.OAuth2ClientCredentials(httpcore.OAuth2Config{
    TokenURL:     tokenSrv.URL,
    ClientID:     "client-id",
    ClientSecret: "client-secret",
})
if err != nil {
    panic(err)
}

apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    fmt.Fprint(w, r.Header.Get("Authorization"))
}))
defer apiSrv.Close()

c, err := httpcore.New(httpcore.Config{
    BaseURL: apiSrv.URL,
    Auth:    auth,
})
if err != nil {
    panic(err)
}

resp, err := c.Do(context.Background(), httpcore.Request{Path: "/config"})
if err != nil {
    panic(err)
}
fmt.Println(string(resp.Body))
// Output: Bearer abc123
```

Two more things about the OAuth2 authenticator are worth knowing before you
rely on it under load or reach for its config afterward:

- **It never holds its lock across the token exchange.** Concurrent callers
  that arrive while a refresh is already in flight share that one exchange
  rather than each starting their own, and a caller waiting on someone else's
  exchange is released by its own context if that context ends first. This
  matters more here than in a general-purpose OAuth2 client: mamori's
  reconciler runs on a single goroutine, so an `Apply` wedged behind a hung
  identity provider would stall reconciliation for every field, not only the
  one currently being resolved.
- **It never retains the `OAuth2Config` it was built from.** The client
  secret lives only inside a closure that encodes the token request body
  once, at construction. This is deliberate, not an oversight: `fmt`'s `%+v`
  and `%#v` walk a struct's unexported fields by reflection, and reflection
  cannot call a `String` method on a value it reaches that way - so a
  redaction method on `OAuth2Config` would not have protected `ClientSecret`
  from a debug dump or a panic trace. Keeping the secret inside a closure,
  which reflection renders as an opaque function pointer, does.

## Error classification

`ClassifyStatus(status int, detail string) error` maps an HTTP status onto a
wrapped mamori error sentinel. It returns `nil` for any 2xx and for 304 (a
successful conditional GET, not a failure):

| HTTP status | mamori kind |
| --- | --- |
| 400 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

`detail` is an optional string the *caller* supplies; `ClassifyStatus` never
reads a response body to build it. A response body can itself contain the
value a ref is resolving - a config value, a secret, a token - so only the
calling provider knows which field, if any, of its backend's own error shape
is safe to surface. Pass a vendor error code or message once you have decided
it cannot contain the resolved value, and pass `""` when in doubt.

`StatusForKind(k mamori.Kind) int` is the exported inverse of
`ClassifyStatus`: given a `mamori.Kind`, it returns an HTTP status that
`ClassifyStatus` maps back to that same kind. It exists for a provider's
conformance test: `providertest`'s `ErrorClassification` case injects a
mamori sentinel, but an HTTP backend's fake transport can only fail a request
with a status code, so a provider's `Fail` hook needs a way to turn that
injected sentinel back into the status that produces it. Hand-rolling that
inverse per provider is exactly what lets the forward table and its inverse
drift apart, and a drifted inverse does not fail loudly - it just makes the
conformance test exercise one classification case five times instead of five
cases once, while still reporting green.

```go
status := httpcore.StatusForKind(mamori.KindRateLimited)
fmt.Println(status)
// Output: 429
```

## Conditional GET

`Revalidator` turns a repeated poll into a conditional GET. mamori polls any
provider without a native watch, so the same ref is fetched on every tick;
`Revalidator` remembers the last `ETag` and `Last-Modified` (and body) for a
key - the ref's raw string, so two fields reading the same ref share one
entry - so the next poll sends `If-None-Match` / `If-Modified-Since` and can
take a 304 with an empty body instead of the full payload again.

`NewRevalidator(c, maxEntries)` bounds the cache at `maxEntries` entries, or
`DefaultRevalidatorEntries` (512) when `maxEntries` is not positive, and
evicts least-recently-used, so a large config cannot grow the cache without
limit. `Get(ctx, key, r)` performs `r` as a conditional GET keyed by `key`. On
a genuine 304 the returned `*Response` carries the cached body with
`NotModified` set, so a caller can treat it exactly like a 200 and still know
nothing changed. A failed request drops the entry, so a later success is
never answered from a validator the backend has not itself confirmed.

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    if r.Header.Get("If-None-Match") == "v1" {
        w.WriteHeader(http.StatusNotModified)
        return
    }
    w.Header().Set("ETag", "v1")
    w.Write([]byte(`{"level":"debug"}`))
}))
defer srv.Close()

c, err := httpcore.New(httpcore.Config{BaseURL: srv.URL})
if err != nil {
    panic(err)
}
rv := httpcore.NewRevalidator(c, 0)

first, err := rv.Get(context.Background(), "cfg", httpcore.Request{Path: "/config"})
if err != nil {
    panic(err)
}
second, err := rv.Get(context.Background(), "cfg", httpcore.Request{Path: "/config"})
if err != nil {
    panic(err)
}
fmt.Println(string(first.Body), second.NotModified, string(second.Body))
// Output: {"level":"debug"} true {"level":"debug"}
```

Two nuances are worth calling out, because each is a decision a reader would
otherwise assume went the other way:

- **On a 304, `Revalidator` reports the validators the CACHE holds, not
  whatever the 304 response itself carried.** RFC 7232 says a 304 should
  repeat `ETag`/`Last-Modified`, but it need not, and real backends - CDNs and
  proxies especially - sometimes omit them. Reporting an empty validator on
  that omission would make `httpcore.Version` fall back to a body hash,
  and mamori would see a "changed" `Version` on a poll that changed nothing:
  a spurious `PreApply`, a spurious `OnChange`, and for a rotating
  credential, a spurious reconnect.
- **The cached body is copied both on the way in and on the way out.** A
  fresh 200's body is copied before it is stored, and a cached body is copied
  again before it is handed to a caller on a 304. Without that second copy,
  the `Revalidator` and every caller that has ever received a given body
  would share one backing array; a caller that decodes or trims its copy in
  place would silently corrupt what the next poll serves.

## What this package does not do

- **No retry.** mamori's reconciler already backs off and retries a failed
  resolve; a second retry layer inside `httpcore` would multiply against it,
  turning a configured five attempts into twenty-five.
- **No vendor error-envelope parsing.** `ClassifyStatus` takes its `detail`
  string from the caller because a response body can contain the resolved
  value itself, and only the provider knows which field of its backend's
  error shape is safe to surface.
- **No SSE.** Server-sent events are a planned, separate capability, not part
  of this package.

## Writing a provider on httpcore

A provider built on `httpcore` typically holds one `*httpcore.Client`,
implements `Scheme` and `Resolve`, and uses `mamori.SelectKey` for `#field`
selection and `httpcore.Version` to derive `Value.Version` from whichever
validator the response actually carried:

```go
// configProvider is a minimal mamori.Provider built on httpcore.Client.
type configProvider struct {
    client *httpcore.Client
}

// newConfigProvider constructs a provider that resolves refs against baseURL
// using a bearer token.
func newConfigProvider(baseURL, token string) (*configProvider, error) {
    c, err := httpcore.New(httpcore.Config{
        BaseURL: baseURL,
        Auth:    httpcore.Bearer(token),
    })
    if err != nil {
        return nil, err
    }
    return &configProvider{client: c}, nil
}

// Scheme returns the URL scheme this provider handles.
func (p *configProvider) Scheme() string { return "example-config" }

// Resolve fetches ref.Path from the backend and selects ref.Key when present.
func (p *configProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
    resp, err := p.client.Do(ctx, httpcore.Request{Path: ref.Path})
    if err != nil {
        return mamori.Value{}, err
    }

    b := resp.Body
    if ref.Key != "" {
        b, err = mamori.SelectKey(b, ref.Key)
        if err != nil {
            return mamori.Value{}, err
        }
    }

    return mamori.Value{
        Bytes:   b,
        Version: httpcore.Version(resp, b),
    }, nil
}
```

Exercised end to end against a fake backend:

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("ETag", "v1")
    w.Write([]byte(`{"level":"debug"}`))
}))
defer srv.Close()

p, err := newConfigProvider(srv.URL, "token")
if err != nil {
    panic(err)
}

v, err := p.Resolve(context.Background(), mamori.Ref{Path: "/config", Key: "level"})
if err != nil {
    panic(err)
}
fmt.Println(string(v.Bytes), v.Version)
// Output: debug v1
```

`Scheme()` must still be registered with `mamori.Register` (typically from an
`init` function in the real package, as every provider in `providers/` does)
before mamori will route a ref to it; that step is omitted above because
`mamori.Register` panics on a duplicate scheme, which would make this
example fail every time it runs alongside the package's other tests.

## Development

This package is its own Go module. Run all commands with the workspace
disabled:

```sh
cd providers/httpcore
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
