# mamori - Cloudflare Workers KV provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from [Cloudflare Workers KV](https://developers.cloudflare.com/kv/),
Cloudflare's low-latency, eventually-consistent key-value store replicated to its
edge network. Cloudflare publishes a Go SDK, but it is not used here: the read
path is a documented HTTPS API, so this provider uses `net/http` and the standard
library only, keeping the SDK's dependency tree out of every consumer's build.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/cloudflare-kv"
```

Importing the package registers the `cloudflare-kv` scheme with mamori. The
provider reads `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and
`CLOUDFLARE_KV_NAMESPACE_ID` lazily at resolve time, so it is safe to register
from a blank import even when no credentials exist at process start.

## Scheme

```
cloudflare-kv://<key>                 key from the configured namespace
cloudflare-kv://<key>?namespace=<id>  explicit namespace
cloudflare-kv://<key>#field           select a field of a JSON-valued key
cloudflare-kv://<key>#/json/pointer   select a nested field by RFC 6901 JSON Pointer
```

- `<key>` - the **entire ref path**, including any slashes it contains. Workers
  KV keys are up to 512 bytes of any printable, non-whitespace character, so a
  key like `config/prod/log-level` is one ordinary key name, not a namespace
  plus a shorter key. That is why the namespace is never taken from the path:
  a segment-count rule like the one `providers/vercel-gc` uses to split
  `<store>/<key>` would silently misread that common shape. The namespace
  comes only from configuration or the ref's `?namespace=` option below.
- `?namespace=<id>` - optional. Overrides the namespace configured via
  `WithNamespaceID` or `CLOUDFLARE_KV_NAMESPACE_ID` for this ref only, letting
  a single provider serve refs that point at different namespaces.
- `#field` / `#/json/pointer` - optional. When present, the stored bytes are
  parsed as JSON and the field is selected via `mamori.SelectKey` (identical to
  every other mamori provider): a fragment starting with `/` is an RFC 6901
  JSON Pointer for nested selection (`#/retry/maxAttempts`); anything else is a
  literal top-level key (`#timeout`).

**A key containing a literal `#` cannot be expressed as a ref.** mamori's ref
grammar parses the `#key` fragment before the `?opts` query, claiming `#` for
field selection - it is not something this provider imposes, and there is no
escape hatch around it. Workers KV itself has no such restriction; a key
actually named `release#42` exists and can be written and read directly against
the API, but it cannot be named by a `cloudflare-kv://` ref. Avoid `#` in a key
you intend to address through mamori.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `cloudflare-kv://log-level` | Value of key `log-level`, in the namespace configured via `WithNamespaceID`/`CLOUDFLARE_KV_NAMESPACE_ID` |
| `cloudflare-kv://config/prod/log-level` | Value of the single key `config/prod/log-level` (the slashes are part of the key, not a path) |
| `cloudflare-kv://log-level?namespace=abcd1234` | Value of `log-level` in the explicit namespace `abcd1234` |
| `cloudflare-kv://api-config#timeout` | Field `timeout` of the JSON-valued key `api-config` |
| `cloudflare-kv://api-config#/retry/maxAttempts` | A nested field of `api-config`, selected by JSON Pointer |

```go
type Config struct {
    LogLevel   string `source:"cloudflare-kv://log-level"`
    MaxRetries int    `source:"cloudflare-kv://max-retries"`
    Region     string `source:"cloudflare-kv://region?namespace=abcd1234"`
    APITimeout string `source:"cloudflare-kv://api-config#timeout"`
}
```

## Fetching a value

A single ref goes through `Resolve`, which issues one authenticated GET against
the single-key value endpoint. That endpoint's response body is the value's
**raw stored bytes**, not a JSON envelope: whatever bytes Cloudflare stored are
the bytes this provider returns, verbatim (after `#field`/`#/json/pointer`
selection, when the ref asks for it).

`mamori.Load` instead calls `ResolveBatch` once, for all the refs in the struct, and
this is where the API's shape flips: the bulk endpoint wraps every value inside
a JSON envelope (`{"result":{"values":{...}}}`), unlike the single-key GET's raw
bytes. This asymmetry is real, not a provider quirk, and it is easy to get
backward if you go looking at the raw HTTP traffic - a single-key read is
already unwrapped; a bulk read is not.

`ResolveBatch` groups refs by namespace, and chunks each namespace's keys to at
most 100 per request - the largest the bulk/get endpoint accepts. A namespace
with 250 referenced keys costs 3 bulk requests (100 + 100 + 50), not one, and
never silently truncates: exceeding the ceiling without chunking would drop
every key past the 100th with no error at all. A key absent from a namespace's
bulk response is simply omitted from the result map, per the `BatchProvider`
contract, so mamori applies that field's default rather than failing the whole
batch; two refs that select different `#field`s of the same key share one slot
in the request rather than costing two.

## Value mapping

Workers KV stores opaque bytes, not a JSON envelope around each value, so there
is no unwrapping step analogous to `providers/vercel-gc`'s JSON-item decoding:
whatever `mamori.SelectKey` returns (or the untouched body, when the ref carries
no `#field`) is the final byte payload.

Two things about the resulting `mamori.Value` are worth stating explicitly,
because each is a decision a reader would otherwise assume went the other way:

- `Value.Sensitive` is always `false`. Cloudflare's own documentation notes that
  anyone with read access to a namespace sees its values in plain text, so this
  provider does not call Workers KV a managed secret store - claiming otherwise
  would overstate the guarantee. Wrap a field in `secret.String` if you want
  redaction anyway.
- `Value.Version` is a content hash (`mamori.VersionHash`) of the resolved
  bytes, not a Cloudflare-issued revision. Workers KV exposes no revision
  number or ETag for a key, so a content hash is the only change signal
  available; it is also all that mamori's polling needs.

`Value.Metadata["namespace"]` records which namespace actually served the
value, which is worth checking when a ref's `?namespace=` override is in play.
This is a deliberate choice, not an oversight sitting next to the credential
hygiene described below: a namespace id is an identifier, not a credential -
it names a bucket, it does not authenticate anything - so the spec blesses
publishing it in `Metadata`. The error-path stripping in the next section is
about not rendering a whole request URL into an error string, not about the
namespace id itself being secret; the two do not contradict each other.

## Error classification

A 404 is detected before status classification runs, on both the single-key
GET path and the bulk POST path, rather than falling into the generic
HTTP-status mapping below - but the two paths do different things with it,
deliberately. `Resolve` returns the detected 404 directly as an error
satisfying `mamori.ErrNotFound`. `ResolveBatch` does not return an error for
it at all: the bulk endpoint has no per-key 404 (a missing key is simply
omitted from a successful response's `values`, as described above), so a 404
there can only mean the namespace itself does not exist, and `ResolveBatch`
treats that exactly like a namespace full of absent keys - it omits every
requested key from the result map, so mamori applies each affected ref's
default, rather than failing the whole call and losing every sibling ref over
one bad namespace.

Beyond 404, every other non-2xx response is classified by HTTP status,
identically on both paths:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 408, 429 | `rate_limited` |
| 400, 422 | `invalid` |
| 5xx, and any other status not named above | `unavailable` |

The mapping is `httpcore.ClassifyStatus`, shared with every other
`httpcore`-backed provider, rather than a table private to this module. An
unrecognized status reports `unavailable` (transient, so mamori backs off and
retries) rather than `unknown`.

**The 404 caveat.** An absent key and an absent namespace are indistinguishable
in Cloudflare's response: both return a plain 404 with no error code this
provider can key on. That means a namespace id that is simply wrong presents
exactly like a namespace full of genuinely absent keys - every field in your
config silently falls back to its default, with no error surfaced anywhere, on
either the single-key or the batch path. If every field unexpectedly defaults
at once, check `Status()` before assuming the keys themselves are missing; a
misconfigured namespace is the more likely cause.

**Credentials never reach an error.** The API token always travels in the
`Authorization: Bearer` header, never a URL or query parameter, so a resolved
value's request cannot leak it into a log line or an error message. The account
id and namespace id are less obviously guarded: both are built into every
request URL, and `http.Client.Do` wraps every transport-level failure - a
refused connection, a timeout, a cancelled context - in a `*url.Error` whose
`Error()` renders that full URL. Without stripping it, an ordinary network
hiccup, not even a bug in this provider, would put the account id and
namespace id into a returned error's text. This provider therefore hands
`httpcore` a `Config.RedactPath` hook that substitutes `<account>` and
`<namespace>` for the two ids, in both their raw and percent-encoded forms,
before `httpcore` composes the message. `httpcore` applies that hook at every
site that renders a URL into an error, including the `*url.Error` it rebuilds
around a transport failure, so the ids cannot be read back out through the
chain either. Substituting the path before the message exists, rather than
rewriting the finished message afterwards as this provider used to, is what
keeps the error chain whole: nothing is rewritten, so `errors.Is(_,
context.Canceled)` and `errors.As(_, &urlErr)` keep working, and the rest of
the path still names which endpoint was called.

## Authentication & configuration

Reading a key requires three things: an API token, an account id, and a
namespace id. Each may be set explicitly or read from the environment; an
explicit option wins over its environment variable. The namespace has one more
source - a ref's `?namespace=` option - which wins over both:

| Source | Option | Environment variable |
| --- | --- | --- |
| API token | `WithAPIToken(token)` | `CLOUDFLARE_API_TOKEN` |
| Account id | `WithAccountID(id)` | `CLOUDFLARE_ACCOUNT_ID` |
| Namespace id | `WithNamespaceID(id)` (ref `?namespace=` wins over this and the env var) | `CLOUDFLARE_KV_NAMESPACE_ID` |

```go
p := cloudflarekv.New(
    cloudflarekv.WithAPIToken(os.Getenv("CLOUDFLARE_API_TOKEN")),
    cloudflarekv.WithAccountID(os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
    cloudflarekv.WithNamespaceID(os.Getenv("CLOUDFLARE_KV_NAMESPACE_ID")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

If any of the three is missing when a ref is resolved, `Resolve` returns an
error naming both the option and the environment variable that would supply
it, and never echoes a credential that is set.

### Options

| Option | Effect |
| --- | --- |
| `WithAPIToken(token)` | Set the Cloudflare API token used to authenticate requests |
| `WithAccountID(id)` | Set the Cloudflare account id that owns the namespace |
| `WithNamespaceID(id)` | Set the default namespace id for refs that carry no `?namespace=` option |
| `WithBaseURL(u)` | Override the API origin, for an `httptest.Server` or a proxy; a trailing slash is trimmed so joining it with a path never produces a double slash |
| `WithHTTPClient(c)` | Inject a custom `*http.Client`; a nil client is a no-op |

`Close()` is idempotent and terminal: after it returns, every `Resolve` and
`ResolveBatch` report `errors.Is(err, mamori.ErrUnavailable)` locally, without
contacting Workers KV. It also returns the HTTP client's idle connections to
the pool, but only when that client's `Transport` is non-nil. `New`'s own
default client (unless overridden with `WithHTTPClient`) leaves `Transport`
unset, and Go resolves a nil `Transport` to the shared `http.DefaultTransport`;
releasing idle connections there would evict connections belonging to
unrelated code in the same process, so `Close` skips it. A client injected
with `WithHTTPClient` is never closed or invalidated either way, only its idle
connections may be released.

## No native watch

The Workers KV REST API exposes no streaming read, no blocking read, and no
digest or ETag this provider could gate a cache on. So unlike
`providers/vercel-gc` - which holds a snapshot behind a cheap digest check and
only refetches the full value when that digest moves - this provider holds no
snapshot at all: every `Resolve` is a live GET against the current value. This
provider deliberately does not implement `mamori.WatchableProvider`, and mamori
wraps it in the polling adapter instead, using `Value.Version` (the content
hash above) to detect a change between ticks.

If many refs share a poll interval and you want to avoid a full round trip on
every tick, compose [`middleware.Cache`](../../middleware/) in front of this
provider to coalesce reads over a TTL you choose.

## Cloudflare Secrets Store is not supported

[Cloudflare Secrets Store](https://developers.cloudflare.com/secrets-store/) is
a distinct Cloudflare product from Workers KV, and this provider does not read
from it - not because it was left out, but because it cannot be added. Secrets
Store values are write-only by design: they are meant to be bound to a Worker at
deploy time, and Cloudflare's API has no endpoint that reads a stored secret's
value back out. There is no HTTPS read path for this provider (or any HTTP
client) to call. If your secrets live in Secrets Store rather than Workers KV,
this module cannot resolve them, and no future version of it will be able to
either.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake HTTP transport (`go test ./...`) |
| Single-key resolution: raw-bytes value shape, namespace precedence (`?namespace=` over `WithNamespaceID` over the environment) | **Verified** (unit tests) |
| A key containing slashes travels as one `url.PathEscape`'d segment, never split into path segments | **Verified** (`TestResolveKeyWithSlashesIsEscapedNotSplit`) |
| `#field` and `#/json/pointer` selection, including selecting a field of a non-object value | **Verified** (unit tests) |
| Not-found (absent key via 404) vs. an absent selected field, both reported as `mamori.ErrNotFound` | **Verified** (unit tests) |
| `ResolveBatch` chunking at the 100-key ceiling, including the exact 100/101 boundary and a 250-key request split into 100+100+50 | **Verified** (`TestResolveBatchChunksAt250`, `TestResolveBatchChunkBoundary`) |
| `ResolveBatch` grouped by namespace; a key deduplicated across refs that select different `#field`s fans out to every ref | **Verified** (`TestResolveBatchGroupsByNamespace`, `TestResolveBatchDedupedKeyFansOutToAllRefs`) |
| An absent key omitted from a batch response leaves its siblings intact, per the `BatchProvider` contract; an invalid selection still fails the batch | **Verified** (unit tests) |
| A 404 namespace on the bulk path omits that namespace's keys instead of failing the batch, and a sibling namespace's ref still resolves | **Verified** (`TestResolveBatchNotFoundNamespaceOmitsKeys`, `TestResolveBatchSurvivesSiblingNamespaceNotFound`) |
| Error classification (401/403/400/422/408/429, and an unnamed status such as 418 as `unavailable`), exercised from both the single-key path and the bulk path | **Verified** (unit tests + `providertest` `ErrorClassification` case) |
| A namespace containing `/` (ref-controlled via `?namespace=`) travels percent-encoded in the request path, on both the single-key and the bulk path | **Verified** (`TestResolveNamespaceWithSlashIsEscaped`, `TestResolveBatchNamespaceWithSlashIsEscaped`) |
| Credentials never reach an error: the token, account id, and namespace id never appear in an error string, on the transport-failure path of both `get` and `bulkGet`, where `httpcore` renders the request URL twice (into its own message and into the `*url.Error` it rebuilds around the cause) | **Verified** (`TestSettingsErrorsNeverCarryCredentials`, `TestResolveSendsBearerTokenNeverInURL`, `TestResolveTransportErrorNeverLeaksCredentials`, `TestResolveBatchTransportErrorNeverLeaksCredentials`, plus `TestRedactPathSubstitutesBothIDs` and `TestRedactPathLeavesAPathCarryingNoIDAlone` on the hook itself) |
| A malformed `WithBaseURL` is rejected at client construction as `mamori.ErrInvalid`, on both the single-key and the bulk path, rather than reaching mamori as an unclassified error it would back off and retry | **Verified** (`TestResolveMalformedBaseURLIsInvalid`, `TestResolveBatchMalformedBaseURLIsInvalid`) |
| No cache: every `Resolve` reaches the fake transport, never a held value | **Verified** (`TestResolveNeverCaches`) |
| Context cancellation | **Verified** (unit tests) |
| `BatchProvider` is implemented; `WatchableProvider` is deliberately not | **Verified** (`TestProviderImplementsBatchProvider`, `TestProviderIsNotWatchable`) |
| `Resolve` and `ResolveBatch` agree on `Version`, `Sensitive`, and `Metadata` for the same ref, not just `Bytes` | **Verified** (`TestResolveBatchOmitsAbsentKeySiblingsSurvive`) |
| End-to-end against a real Workers KV namespace, including that `Resolve` and `ResolveBatch` agree on the same key despite the raw-bytes-vs-envelope asymmetry | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that emulates both Workers
KV endpoints (single-key GET and bulk POST, including injectable per-namespace
failures), so `go test ./...` requires **no** network access and **no**
Cloudflare credentials.

### Live integration test

An integration test exercises the real Workers KV REST API. It is guarded by a
build tag and skips unless `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`,
`CLOUDFLARE_KV_NAMESPACE_ID`, and `CLOUDFLARE_KV_TEST_KEY` (the key of an
existing entry) are all set. It cannot create keys - that would need
Cloudflare's write API - so it verifies the read path, and it is also the only
way to confirm that this provider's bulk-response parsing actually agrees with
what Cloudflare sends in production, since a fake can only confirm agreement
with its own encoding of that envelope, not the real one:

```sh
export CLOUDFLARE_API_TOKEN=...
export CLOUDFLARE_ACCOUNT_ID=...
export CLOUDFLARE_KV_NAMESPACE_ID=...
export CLOUDFLARE_KV_TEST_KEY=some-existing-key
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace
disabled:

```sh
cd providers/cloudflare-kv
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
