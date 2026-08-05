# mamori - Alibaba Nacos provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves
configuration from the [Nacos](https://nacos.io/) configuration service, with
**native hot-reload** driven by Nacos's long-poll listener.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/nacos"
```

Importing the package registers the `nacos` scheme with mamori. The HTTP client
is built lazily on first use from the ambient `NACOS_*` environment, so importing
the package never contacts a server.

This module speaks Nacos's Open API directly over the standard library. It does
**not** use `nacos-sdk-go`: the two endpoints it needs are plain HTTP, and the
whole module's dependency set is the standard library, `mamori`, and
[`providers/httpcore`](../httpcore/).

## Scheme

```
nacos://[<group>/]<dataId>[#json-key]
```

- `<dataId>` - the configuration's dataId, e.g. `application.properties`. Dots
  are ordinary characters, so `com.example.svc.yaml` is one dataId.
- `<group>` - optional. When absent, the provider's group is used, which
  defaults to Nacos's own `DEFAULT_GROUP`.
- `#json-key` - optional. When present the configuration is parsed as JSON and
  the named field is selected via `mamori.SelectKey`, the same behaviour as every
  other mamori provider. A fragment beginning with `/` is an RFC 6901 JSON
  Pointer.

A path with more than two segments is rejected with `mamori.ErrInvalid` rather
than guessed at, so `mamori doctor` catches the typo before deployment.

**The namespace is not in the ref.** A Nacos configuration is addressed by
namespace, group, and dataId, but the namespace is on the `Provider`
(`WithNamespace`, `NACOS_NAMESPACE`) because it is the boundary a set of
credentials is issued against: one server, one namespace, one login. A ref that
could name any namespace would make every struct tag able to reach another
tenant's configuration whenever the credentials happen to span both.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `nacos://application.properties` | The whole configuration `application.properties` in `DEFAULT_GROUP` |
| `nacos://prod/db.json#password` | Field `password` of the JSON at dataId `db.json` in group `prod` |
| `nacos://app.json#/database/dsn` | RFC 6901 pointer into `app.json` in the default group |

```go
type Config struct {
    LogLevel   string `source:"nacos://app.json#log_level"`
    DBHost     string `source:"nacos://prod/db.json#host"`
    DBPassword string `source:"nacos://prod/db.json#password"`
    Raw        string `source:"nacos://application.properties"`
}
```

### Value semantics

- `Value.Bytes` - **the raw response body.** Nacos's v1 read endpoint answers
  with the configuration content itself, with no JSON envelope, which is unusual
  among mamori's HTTP-backed providers. Nothing is unwrapped; a `#json-key` then
  selects out of those bytes.
- `Value.Version` - `mamori.VersionHash` over the bytes actually returned.
  Nacos sends no `ETag`, and its `Last-Modified` has one-second resolution, so
  two publishes inside the same second are indistinguishable through it - during
  a rollout, where a bad config is often corrected seconds after it is pushed,
  that would make mamori skip the correction entirely. Hashing the **selected**
  bytes rather than the whole document is equally deliberate: a field bound to
  `#log_level` must not report a change because an unrelated key moved.
- `Value.Sensitive` - always `false`. Nacos holds application configuration
  rather than managed secrets. Wrap a field in `secret.String` for redaction
  anyway.
- A configuration that does not exist returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`, so the field's `default:` applies.

## Error classification

Every failure goes through `httpcore.ClassifyStatus`, so this provider's mapping
is the shared one:

| HTTP status | mamori kind | Source |
| --- | --- | --- |
| 400 | `invalid` | Documented by Nacos for both endpoints |
| 401 | `unauthenticated` | General HTTP mapping, inherited from `httpcore` |
| 403 | `permission_denied` | Documented by Nacos for both endpoints |
| 404 | `not_found` | Documented by Nacos; the server writes the body `config data not exist` |
| 408, 429 | `rate_limited` | General HTTP mapping, inherited from `httpcore` |
| 409 | `unavailable` | Not in the Nacos error table; the server writes `requested file is being modified, please try later.` (`ConfigServletInner.doGetConfig`) |
| 422 | `invalid` | General HTTP mapping, inherited from `httpcore` |
| anything else, incl. 500 | `unavailable` | 500 is documented by Nacos; the rest is the default |

**No response body ever reaches an error.** `httpcore`'s `ErrorDetail` hook is
deliberately left nil, because on a 200 that same body *is* the configuration:
there is no envelope field this provider could select and be certain it is not
the value. The status classification is the answer, and the error names the
coordinates (`dataId=...  group=...`) so a wrong group is distinguishable from a
wrong dataId, which is the mistake a Nacos user actually makes.

`errors_test.go` drives every row above through a real `Resolve` and asserts the
kind a mamori user sees, separately from the conformance kit.

## Authentication & configuration

| Setting | Option | Environment | Default |
| --- | --- | --- | --- |
| Server address | `WithServerAddr` | `NACOS_SERVER_ADDR` | `http://127.0.0.1:8848` |
| Servlet context path | `WithContextPath` | `NACOS_CONTEXT_PATH` | `/nacos` |
| Namespace (`tenant`) | `WithNamespace` | `NACOS_NAMESPACE` | empty, i.e. `public` |
| Default group | `WithGroup` | `NACOS_GROUP` | `DEFAULT_GROUP` |
| Username / password | `WithCredentials` | `NACOS_USERNAME` / `NACOS_PASSWORD` | none |
| Arbitrary authenticator | `WithAuth` | - | none |
| HTTP client | `WithHTTPClient` | - | one whose timeout outlasts the long-poll hold |
| Long-poll hold | `WithLongPollTimeout` | - | 30s |
| Response ceiling | `WithMaxBody` | - | 1 MiB |

```go
p := nacos.New(
    nacos.WithServerAddr("http://nacos.svc.cluster.local:8848"),
    nacos.WithNamespace("2f9d1b0c-..."),
    nacos.WithCredentials(os.Getenv("NACOS_USERNAME"), os.Getenv("NACOS_PASSWORD")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Supplying only one of username and password is a configuration error rather
than a silent fallback to unauthenticated requests, which would work against a
server with auth disabled and fail with an opaque 403 against every other one.

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and
any `Watch` started after `Close`, report
`errors.Is(err, mamori.ErrUnavailable)` locally, without contacting the Nacos
server. Without `WithHTTPClient` it releases nothing: the
client built for that unconfigured case belongs to an internal helper, not to a
field `Close` holds a reference to. A client injected with `WithHTTPClient` is
never closed or invalidated, only its idle connections are returned to the
pool, and only when that client's `Transport` is non-nil (a nil `Transport`
resolves to the shared `http.DefaultTransport`, which `Close` leaves alone).

A `Watch` that was **already running** when `Close` was called is a different
case: `Close` does not end it. Here it degrades to an error stream rather than
going quiet, because every long-poll round re-enters the same closed check
before it sends anything, so each round reports
`errors.Is(err, mamori.ErrUnavailable)` as an error update and the loop keeps
going until that watch's own context is cancelled. Cancelling that context is
the only way to stop it. See
[Close does not stop a Watch](https://mamorigo.dev/docs/writing-a-provider/#close-does-not-stop-a-watch)
for what every other provider does here.

### The access token travels in the query string

Nacos's documented way to carry a token is a query parameter:
"`accessToken=${accessToken}` should be appended at the end of request url". This
provider does that, and it is worth knowing what it costs: a query parameter is
in the request line, which a proxy's access log and the server's own request log
record in plaintext, and Nacos's stock deployment is cleartext `http` on port
8848. Put a TLS terminator in front of Nacos and set `NACOS_SERVER_ADDR` to the
`https://` address if the network between your application and Nacos is not
already private.

What this module does guarantee: neither the password nor the token is held in
any readable struct field (both live inside closures, so `%+v` and `%#v`, which
walk unexported fields by reflection and cannot call a `String` method on what
they reach, render them as opaque function pointers), and `httpcore` strips the
query from every error it returns - including the `*url.Error` `net/http` wraps
a transport failure in, which otherwise renders the full URL. A test asserts
that neither the token nor the password appears in an error's text.

### Other authentication modes

Nacos's auth is pluggable and this module implements one mode natively. The
others are header injection, which is exactly what an `httpcore.Authenticator`
is, so `WithAuth` is the seam:

- **Username / password** (the built-in `nacos` auth plugin) - implemented here.
  `POST /nacos/v1/auth/login` with `username` and `password`, returning
  `{"accessToken": ..., "tokenTtl": ...}`. The token is cached until a minute
  before its stated TTL; a response that states no TTL is not cached at all.
- **accessKey / secretKey** - the signature scheme Alibaba Cloud's hosted MSE
  Nacos issues. Pass your own `httpcore.Authenticator` to `WithAuth`.
- **Server identity header** - `nacos.core.auth.server.identity.key` /
  `.value`, for server-to-server calls. `httpcore.HeaderAuth(key, value)`.
- **No authentication** - the default when `nacos.core.auth.enabled` is false.
  Supply no credentials.

## Native watch (long-poll listener)

`*Provider` implements `mamori.WatchableProvider`, so mamori subscribes directly
instead of wrapping this provider in its polling adapter.

Nacos's change notification is neither a stream nor a blocking read of the value.
It is a **comparison**. One round POSTs "here is the MD5 I believe this
configuration currently has", and the server holds the request open until either
that belief becomes wrong or the hold elapses. Nothing is pushed, and the
response names only *which* configuration moved, never its content, so a round
that reports a change is followed by an ordinary read.

The loop is `httpcore.LongPoll`: one goroutine, one round at a time, a client
deadline strictly longer than the hold the server was given, closure on context
cancellation, and no re-attempt of a round already reported. This provider
supplies only the two Nacos-specific halves - what a baseline is, and what a
round sends.

### The wire format, spelled out

The probe is the value of the `Listening-Configs` form field on
`POST /nacos/v1/cs/configs/listener`:

```
dataId  0x02  group  0x02  contentMD5  0x02  tenant  0x01
```

with the tenant field and its preceding separator omitted when there is no
namespace, which is the alternative form the Open API gives
(`dataId^2group^2contentMD5^1`). `0x02` is ASCII STX and `0x01` is ASCII SOH,
both invisible; because the probe is an ordinary
`application/x-www-form-urlencoded` value they reach the wire as `%02` and `%01`.
The header `Long-Pulling-Timeout: 30000` says how long to hold, in milliseconds.

**`Long-Pulling-Timeout` is spelled that way on purpose.** It is Nacos's own
spelling (`LONG_POLLING_HEADER` in `LongPollingService.java`), and
`isSupportLongPolling` decides whether to park a request purely on whether that
header is present. Correcting the spelling does not break the watch - Nacos falls
back to *short* polling and answers immediately either way - which is exactly why
it is dangerous: every behavioural test would still pass while the loop hammered
the server as fast as it could. Only an assertion on the request itself catches
that, and one exists.

The response is the part most easily got wrong. The server builds

```
dataId  0x02  group  0x02  tenant  0x01
```

and then returns `URLEncoder.encode(that, "UTF-8")`
(`MD5Util.compareMd5ResultString`). The body on the wire therefore carries the
**literal characters** `%02` and `%01`, not the control bytes. Splitting the raw
body on `\x01` finds nothing, so the watch would report "unchanged" forever: it
would never fire, never error, and never log. This provider URL-decodes first,
tolerates the un-encoded form as a fallback for whatever proxy sits in front of
the endpoint, and has an explicit test asserting a change is detected from the
encoded shape.

An empty body means the hold elapsed with nothing to report, which is the
ordinary outcome several times an hour for every watched configuration.

### Baseline ordering, and why `WatchDeliversBaseline` is honest here

`Watch` reads the configuration once, remembers its MD5, and emits that value as
a baseline before the first listener round. `providertest.Config`'s
`WatchDeliversBaseline` asks for more than a baseline: it asks that the
subscription already be established when the baseline is emitted, so a write
landing right after it cannot fall into a gap.

A comparison-based protocol satisfies that without any coordination. The baseline
read and the MD5 the first round subscribes with come from the same response, so
a write landing between them simply makes that round's MD5 stale, and Nacos
answers it immediately rather than parking it. A stream-based watch has to attach
before it reads, because its notifications only cover what happens afterwards; a
comparison-based one carries the "before" with it in every request. The field is
therefore declared, in both the unit and the integration conformance runs.

### Deletion

A configuration that disappears emits nothing, matching mamori's polling adapter,
which is silent for a key that does not exist - the field keeps its last good
value rather than being handed an empty one. The watch resets its remembered MD5
to the empty digest, which is what a Nacos client sends for content it does not
hold, so the round parks properly and a republish fires. A test asserts both
halves, including that the loop is still parking rather than spinning.

## Which Nacos version this targets

**Nacos 2.x, using the v1 Open API, and it works unchanged against a 1.x server.**

The reason is the listener. Nacos publishes an HTTP long-poll listener only at
v1 (`/nacos/v1/cs/configs/listener`). The v2 API added a JSON envelope for reads
(`/nacos/v2/cs/config`, answering `{"code":0,"message":"success","data":"..."}`)
and moved change notification to gRPC, which this module cannot speak within its
dependency budget. Nacos's own documentation states that "Nacos 2.X is compatible
with Nacos 1.X OpenAPI", so the v1 read and the v1 listener are one coherent pair
with one response shape and one error vocabulary.

Nacos 3.0 rebuilt the admin surface (`/nacos/v3/admin/cs/config/...`), but its
`config/listener` endpoint is a *query for which clients are listening*, not a
long-poll listener, and is not a replacement for this. If you run 3.x, verify
that the v1 compatibility endpoints are enabled on your deployment before relying
on this provider; that combination is documented but not live-verified here.

## Documentation this provider is built from

Everything above is taken from Nacos's own documentation and, where the docs are
silent or ambiguous, from the server implementation. Rows marked below as
*documented* are quoted from the vendor docs; rows marked *from the server
source* are behaviour the docs do not state.

| Fact | Where it comes from |
| --- | --- |
| Read endpoint, `dataId` / `group` / `tenant`, raw body response, error table | [Open API Guide (1.X)](https://nacos.io/en/docs/1.X/open-api/), [Open API Guide (v2)](https://nacos.io/en/docs/v2/guide/user/open-api/) - documented |
| Listener endpoint, `Listening-Configs` format, `^1` / `^2` separators and their `%01` / `%02` encodings, `Long-Pulling-Timeout: 30000` | [Open API Guide (1.X)](https://nacos.io/en/docs/1.X/open-api/) - documented |
| Response shape on change, `dataId%02group%02tenant%01` | [Open API Guide (1.X)](https://nacos.io/en/docs/1.X/open-api/) - documented |
| That the response is `URLEncoder.encode`d as a whole, so the body carries literal `%02` / `%01` | [`MD5Util.compareMd5ResultString`](https://github.com/alibaba/nacos/blob/develop/config/src/main/java/com/alibaba/nacos/config/server/utils/MD5Util.java) - from the server source; the doc's example shows the encoded form but never says it is encoded |
| That a missing `Long-Pulling-Timeout` header degrades to short polling rather than failing | [`LongPollingService.isSupportLongPolling`](https://github.com/alibaba/nacos/blob/develop/config/src/main/java/com/alibaba/nacos/config/server/service/LongPollingService.java) - from the server source |
| Login endpoint, `accessToken` / `tokenTtl`, token as a query parameter | [Authentication](https://nacos.io/en/docs/v2/guide/user/auth/) - documented |
| Other auth modes (accessKey/secretKey, server identity) | [Authorization Plugin](https://nacos.io/en/docs/v2/plugin/auth-plugin/) - documented, not implemented here |
| 404 body `config data not exist`, 409 body `requested file is being modified, please try later.` | `ConfigServletInner.doGetConfig` - from the server source; 409 is absent from the doc's error table |
| 1.X / 2.X OpenAPI compatibility | [Open API Guide (v2)](https://nacos.io/en/docs/v2/guide/user/open-api/) - documented |

**One ambiguity worth stating plainly.** The Open API page gives the
`Listening-Configs` format in a prose form that renders inconsistently across the
doc's several versions - `dataId^2group^2contentMD5^2tenant^1` in some renderings
and a garbled variant in others - while the same page's worked example is
unambiguous: `Listening-Configs=dataId%02group%02contentMD5%02tenant%01`. This
module follows the example, cross-checked against the server's own parser, and
pins it with a test that asserts the exact encoded bytes. Getting it wrong
produces a watch that silently never fires, so it is not a detail to infer.

## Testing status

| Area | How it is verified |
| --- | --- |
| `Resolve`, raw body, `#key` selection, JSON Pointer, versioning | Unit tests and `providertest.Run` against an in-process `http.RoundTripper` fake |
| Error classification | `errors_test.go`, a table over every documented status, driven through a real `Resolve` |
| Long-poll wire format | Asserted byte-for-byte against the encoded probe, including the `%02` / `%01` separators and the header spelling |
| Native watch | `providertest.Run` runs `WatchEmitsOnMutate` and `WatchClosesOnCancel` for real; `SkipWatch` is unset and a separate test fails if either case ever *skips* |
| The watch is not vacuous | A test builds a listener that never reports a change and asserts the watch harness fails against it |
| Goroutine hygiene | `providertest`'s `NoGoroutineLeak` (`goleak.VerifyNone`), plus `go test -race` |
| Live Nacos | `//go:build integration`, see below |

The fake is an in-process `http.RoundTripper`, never an `httptest.Server`: the
conformance kit's `NoGoroutineLeak` case snapshots goroutines before the first
subtest and a live server's accept goroutine does not survive that snapshot. The
fake also checks `req.Context().Err()` on every request, so a provider that
failed to thread the context into the request it built could not pass.

### Live integration test

```sh
docker run --rm -p 8848:8848 -e MODE=standalone nacos/nacos-server:v2.4.3
export MAMORI_NACOS_ADDR=http://127.0.0.1:8848
# optional, when auth is enabled on the server:
export MAMORI_NACOS_USERNAME=nacos MAMORI_NACOS_PASSWORD=nacos
# optional, to run in a namespace other than public:
export MAMORI_NACOS_NAMESPACE=<namespace-id>
GOWORK=off go test -tags integration -run Integration ./...
```

It skips cleanly when `MAMORI_NACOS_ADDR` is unset, so
`go test -tags integration ./...` is safe to run anywhere. Besides the
conformance kit, it exercises the listener against the real implementation of
the protocol, which is the one thing a fake cannot settle: whether this
package's reading of the separators, the header spelling, and the response
encoding matches what Nacos actually does.

## Development

This package is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/nacos
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -race ./...
GOWORK=off go vet -tags integration ./...
```
