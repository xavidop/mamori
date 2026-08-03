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

### `Request.Path` is an escaped path

`Path` is joined onto `BaseURL` in its **escaped** form, the one `net/url`
calls `RawPath`, so a caller can name a single segment whose own name contains
a slash. Here the backend echoes the request URI it actually received, which is
the only thing that settles whether an escape survived:

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    fmt.Fprint(w, r.URL.RequestURI())
}))
defer srv.Close()

c, err := httpcore.New(httpcore.Config{BaseURL: srv.URL + "/v1"})
if err != nil {
    panic(err)
}

resp, err := c.Do(context.Background(), httpcore.Request{
    Path: url.PathEscape("config/prod/log-level"),
})
if err != nil {
    panic(err)
}
fmt.Println(string(resp.Body))
// Output: /v1/config%2Fprod%2Flog-level
```

One segment, not three, and not the double-encoded
`config%252Fprod%252Flog-level` that writing into `url.URL.Path` alone
produces. Cloudflare Workers KV keys, to take the case that forced this, are up
to 512 bytes of any printable non-whitespace characters, so
`config/prod/log-level` is one ordinary key name and a provider for it cannot
address a key at all without the distinction.

The cost is that a literal percent sign must be written `%25`. A `Path` that is
not a valid escaped path is rejected with `mamori.ErrInvalid` rather than
guessed at, because guessing would make what a ref means depend on whether its
escapes happened to parse.

### `Do` refuses a path that escapes the `BaseURL`

**`Do` rejects a `Request.Path` containing a `.` or `..` segment, wrapping
`mamori.ErrInvalid`, before anything is sent.** Every provider built on
`httpcore` inherits that, and none can forget it. That placement is the whole
point: `joinPath` does not resolve dot segments, so `../..` in a
caller-supplied path reaches outside the path prefix the `BaseURL` declares -
for a client scoped to `https://api.example.com/v1/tenants/acme`, that is
another tenant's configuration, reached without ever leaving the declared host
and so without tripping whatever host or endpoint restriction the provider
relies on to contain exactly this.

It is reachable, not theoretical: a provider's path comes from a mamori ref,
and a ref path is not only what the struct tag says. `${VAR}` interpolation
substitutes values the application supplies at runtime, so a ref of
`https://billing/${TENANT}/cfg` carries whatever `TENANT` holds.

Four details are deliberate:

- **It rejects rather than cleans.** Resolving `..` away would silently change
  which value a ref names, and a ref that quietly means something other than
  what it says is worse than one that fails. `mamori doctor` resolves every ref
  before deployment, so a rejected ref surfaces there rather than in production.
- **It splits on `\` as well as `/`.** Splitting on `/` alone leaves
  `a\..\..\secrets` as one segment matching neither `.` nor `..`, so it would
  pass, and the wire would carry `%5C` - which IIS and ASP.NET decode and honor
  as a directory separator, the classic backslash traversal bypass. A `BaseURL`
  is operator-supplied with no platform restriction, so this package cannot
  assume the backend is not one of them. A backslash in an ordinary key still
  works; only a traversal is refused.
- **It runs on the decoded path**, so `%2e%2e` is refused exactly like `..`.
  That check is needed precisely because `Path` is an escaped path: preserving
  a caller's escapes so an encoded slash survives would otherwise preserve an
  encoded traversal with it.
- **`...` is not a traversal.** RFC 3986 section 5.2.4 defines dot-segment
  removal over exactly `.` and `..`, so a three-dot segment is an ordinary name.

No server is needed to show it: every one of these is refused before a request
is even built.

```go
c, err := httpcore.New(httpcore.Config{
    BaseURL: "https://api.example.com/v1/tenants/acme",
})
if err != nil {
    panic(err)
}

for _, path := range []string{
    "../../other-tenant/cfg",
    `a\..\..\secrets`,
    "%2e%2e/secrets",
} {
    _, err := c.Do(context.Background(), httpcore.Request{Path: path})
    fmt.Println(errors.Is(err, mamori.ErrInvalid))
}
// Output:
// true
// true
// true
```

`Do` can fail for several distinct reasons: request construction (a
malformed or traversing path, or an `Authenticator` whose `Apply` itself
errors),
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

`TokenURL` must be `https://`. The client-credentials grant POSTs
`client_secret` in the form body, so a cleartext token endpoint hands that
secret to anything on the path, and the secret is the key to every value the
backend serves. The scheme is checked against a closed set, so an `ftp://`
typo or a `ws://` paste fails at construction rather than on every exchange.
`OAuth2Config.AllowInsecure` opts into `http://` for a local test identity
provider, exactly like `Endpoint.AllowInsecure` in `providers/https`: it
permits cleartext `http` and nothing else, never a way to skip the scheme
check itself.

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
    // httptest serves http://, which is exactly what AllowInsecure is for.
    // A real identity provider needs https:// and must not set this.
    AllowInsecure: true,
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
- **It holds no credential in a readable field, neither the client secret nor
  the access token.** The secret lives only inside a closure that encodes the
  token request body once, at construction, and the cached access token lives
  inside a closure created by each successful exchange. This is deliberate,
  not an oversight: `fmt`'s `%+v` and `%#v` walk a struct's unexported fields
  by reflection, and reflection cannot call a `String` or `GoString` method on
  a value it reaches that way - so a redaction method, on `OAuth2Config` or on
  an opaque token wrapper, would not have protected either one from a debug
  dump or a panic trace; `fmt` falls back to printing the raw contents.
  Keeping both inside closures, which reflection renders as opaque function
  pointers, does. The access token earns that treatment as much as the secret:
  it is a live bearer credential for the whole backend until it expires.

## Error classification

`ClassifyStatus(status int, detail string) error` maps an HTTP status onto a
wrapped mamori error sentinel. It returns `nil` for any 2xx and for 304 (a
successful conditional GET, not a failure):

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

422 Unprocessable Entity is named explicitly rather than left to the default,
and the reason is worth stating: the default kind is *transient*, so mamori
backs off and retries. A 422 means the request was well formed and
semantically wrong, which retrying can never fix, and at least one backend in
this ecosystem (Infisical) answers with it.

A status `http.StatusText` does not know is rendered without its name rather
than with an empty one, so a Cloudflare 520 reads `httpcore: http 520:
mamori: unavailable` and not `http 520 : ...`. `ClassifyStatus` carries the
`httpcore:` prefix, like every other error this package returns, because a
provider calling it directly - the documented pattern - would otherwise
surface an unattributed message; `Do` adds the prefix itself and so calls an
unexported twin, keeping one copy of the table and one copy of the prefix.

### Supplying `detail`

`detail` is an optional string the *caller* supplies; `ClassifyStatus` never
reads a response body to build it. A response body can itself contain the
value a ref is resolving - a config value, a secret, a token - so only the
calling provider knows which field, if any, of its backend's own error shape
is safe to surface. Pass a vendor error code or message once you have decided
it cannot contain the resolved value, and pass `""` when in doubt.

`Config.ErrorDetail` is how that string reaches `Do`, which would otherwise
have no channel for it at all - the body is drained and the response is gone
by the time `Do` returns:

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusForbidden)
    w.Write([]byte(`{"error":{"code":"token_scope_missing","value":"s3cr3t"}}`))
}))
defer srv.Close()

c, err := httpcore.New(httpcore.Config{
    BaseURL: srv.URL,
    ErrorDetail: func(status int, body []byte) string {
        // Only the fields you have decided cannot carry the resolved value.
        var env struct {
            Error struct {
                Code string `json:"code"`
            } `json:"error"`
        }
        if json.Unmarshal(body, &env) != nil {
            return ""
        }
        return env.Error.Code
    },
})
if err != nil {
    panic(err)
}

_, err = c.Do(context.Background(), httpcore.Request{Path: "/config"})
fmt.Println(errors.Is(err, mamori.ErrPermissionDenied))
fmt.Println(strings.Contains(err.Error(), "token_scope_missing"))
// The sibling field the hook did not select never reaches the message.
fmt.Println(strings.Contains(err.Error(), "s3cr3t"))
// Output:
// true
// true
// false
```

Three things about it:

- **It is called only for a status `ClassifyStatus` rejects.** On a success the
  body *is* the resolved value and belongs to the caller, not to an error
  message.
- **It sees at most `MaxBody` bytes**, truncated rather than rejected, so a
  backend that answers `403` with a gigabyte of HTML can neither defeat the
  memory ceiling nor turn its own `403` into a "response too large" error.
- **Nil means no detail, and nil is the safe default.** A body can be the
  secret. Leaving the hook unset is a guarantee that no response body ever
  reaches an error, and it is what `providers/https` does.

It exists because several providers already embed a bounded error body in
their messages - a house convention shared by `providers/doppler`,
`providers/cloudflare-kv`, `providers/vercel-gc` and `providers/scaleway-sm` -
and a migration onto `httpcore` has to be able to preserve each provider's
error mapping exactly.

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
- **A 304 with no cached body to pair it with is an error, never an empty
  one.** If the entry was evicted mid-flight, `Get` retries unconditionally and
  uses the fresh body. If the backend answers 304 to *that* retry, which
  carried no validators at all, it is violating RFC 7232 and `Get` fails with
  `mamori.ErrUnavailable` rather than returning `Body: nil` with a nil error.
  That distinction matters most on the path it is easiest to overlook: the very
  first poll of a key, where there is no cache entry by definition. Returning
  the empty body there would have mamori apply an empty string as though it
  were the resolved value.

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
validator the response actually carried.

Handing `ref.Path` straight to `Request.Path`, as the example below does, is
safe: `Do` rejects a `.` or `..` segment itself, in either its literal or its
percent-encoded form, so a ref cannot escape the prefix your `BaseURL`
declares. That is deliberately not something a provider has to remember - see
"`Do` refuses a path that escapes the `BaseURL`" above. If your backend's key
names may themselves contain slashes, escape the path first
(`url.PathEscape(key)`) so the whole key stays one segment.

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
//
// ref.Path needs no traversal check here: Do rejects a "." or ".." segment,
// literal or percent-encoded, before it sends anything.
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
