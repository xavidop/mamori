# mamori - Vercel Global Config provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from [Vercel Global Config](https://vercel.com/docs/global-config) (formerly
Edge Config), the globally replicated key-value store Vercel applications read at
runtime for feature flags, redirect maps, and experimentation settings. Vercel
publishes no Go SDK, and none is needed: the read path is a documented HTTPS API, so
this provider uses `net/http` and the standard library only.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/vercel-gc"
```

Importing the package registers the `vercel-gc` scheme with mamori. The provider
reads `GLOBAL_CONFIG` (falling back to `EDGE_CONFIG`) lazily at resolve time, so it
is safe to register from a blank import even when no credentials exist at process
start.

## Scheme

```
vercel-gc://<key>                store and token from the connection string
vercel-gc://<store-id>/<key>     explicit store
vercel-gc://<key>#field          select a field of a JSON-valued key
vercel-gc://<key>#/json/pointer  select a nested field by RFC 6901 JSON Pointer
```

- `<key>` - the Global Config key, e.g. `log-level`. One path segment is a key
  resolved against the store named by the connection string; two segments are
  `<store-id>/<key>`, naming the store explicitly. This is unambiguous because a
  Global Config key can never contain a slash.
- `#field` / `#/json/pointer` - optional. When present, the stored value is parsed
  as JSON and the field is selected via `mamori.SelectKey` (identical to every other
  mamori provider): a fragment starting with `/` is an RFC 6901 JSON Pointer for
  nested selection (`#/retry/maxAttempts`); anything else is a literal top-level key
  (`#timeout`).

### Ref examples

| Ref | Meaning |
| --- | --- |
| `vercel-gc://log-level` | Value of key `log-level`, in the store named by `GLOBAL_CONFIG`/`EDGE_CONFIG` |
| `vercel-gc://ecfg_abc123/log-level` | Value of `log-level` in the explicit store `ecfg_abc123` |
| `vercel-gc://api-config#timeout` | Field `timeout` of the JSON-valued key `api-config` |
| `vercel-gc://api-config#/retry/maxAttempts` | A nested field of `api-config`, selected by JSON Pointer |

```go
type Config struct {
    LogLevel   string `source:"vercel-gc://log-level"`
    MaxRetries int    `source:"vercel-gc://max-retries"`
    Checkout   bool   `source:"vercel-gc://ecfg_abc123/checkout-v2-enabled"`
    APITimeout string `source:"vercel-gc://api-config#timeout"`
}
```

### How stored values map to config values

| Stored value | Bytes |
| --- | --- |
| string | the raw text, unquoted |
| boolean | `true` or `false` |
| number | its JSON text, e.g. `5432` or `0.25` |
| object or array | its compacted JSON encoding |
| null | `null`, because a key stored as null exists |

Three things are worth stating explicitly, because each is a decision a reader
would otherwise assume went the other way:

- `Value.Sensitive` is always `false`. Global Config holds flags, redirects, and
  blocklists, not managed secrets. Wrap a field in `secret.String` if you want
  redaction anyway.
- `Value.Version` is a content hash (`mamori.VersionHash`) of the resolved bytes,
  not the store digest. The digest moves on any edit to any key in the store, so
  using it as `Version` would fire a spurious change event for every unrelated
  field on every unrelated edit. The digest is still reported, in `Metadata["digest"]`.
- There is no cache TTL. Every `Resolve` makes a digest request, so `mamori.Refresh`
  and `mamori.Doctor` always reach Vercel rather than being satisfied by a
  time-based cache that could return a held value without contacting the backend at
  all.

Unlike the LaunchDarkly provider, which evaluates a flag through the LaunchDarkly
SDK's own value type - representing every JSON number as a `float64` before
converting it back to decimal text - this provider never parses a stored number
into a Go numeric type at all. The stored value stays a `json.RawMessage` end to
end and is only compacted, not re-encoded, so the digits returned are exactly the
digits Global Config stored, with no float64 round trip and no precision loss for
an integer wider than a float64 mantissa can represent exactly.

## Error classification

A store that does not exist returns HTTP 404 from every endpoint, mapped directly
to `mamori.ErrNotFound`. A key that does not exist in a store that does exist is
detected by its absence from the fetched item map, not by a status code, and also
reports `mamori.ErrNotFound`. Beyond those two not-found cases, every other
non-2xx response is classified by HTTP status:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 429 | `rate_limited` |
| 400 | `invalid` |
| 5xx | `unavailable` |
| anything else | `unknown` |

One caveat is worth stating plainly rather than leaving implicit: Vercel's
documented error body for a request missing an authentication token carries
`"code": "forbidden"`, so a 403 from this API can mean an absent credential rather
than an insufficient one. Vercel has not published the full error-code
vocabulary, so the mapping keys on status rather than guessing at a body it
cannot rely on.

## Authentication & configuration

Connecting a Global Config store to a Vercel project sets `GLOBAL_CONFIG` in the
project's environment to a connection string of the form:

```
https://global-config.vercel.com/<store-id>?token=<read-token>
```

Stores connected before Vercel renamed Edge Config to Global Config instead set
`EDGE_CONFIG`, pointing at `edge-config.vercel.com`. This provider reads
`GLOBAL_CONFIG` first and falls back to `EDGE_CONFIG`, taking the host from the
connection string rather than hardcoding it - which is what keeps the legacy
origin working.

The token always travels in the `Authorization: Bearer` header, never the
documented `token` query parameter, so a request to the Global Config API cannot
leak it into a log line, a trace span, or an error message. That guarantee covers
requests, not parsing: the token is still part of the connection string itself, so
a malformed connection string (for example a trailing newline picked up from a
file or `kubectl create secret --from-file`) can surface it inside the
`url.Parse` diagnostic, which this provider strips before returning the error.
The other place it can leak is self-inflicted: passing a full connection string
to `WithBaseURL` by mistake - plausible, since `WithConnectionString` exists too -
puts the token in the host, and therefore in every transport error.

Precedence, most specific first:

1. `WithConnectionString(...)`
2. `WithStoreID(...)` / `WithToken(...)`
3. `GLOBAL_CONFIG`
4. `EDGE_CONFIG`

```go
p := vercelgc.New(vercelgc.WithConnectionString(os.Getenv("GLOBAL_CONFIG")))
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

or point at a specific store and token directly:

```go
p := vercelgc.New(
    vercelgc.WithStoreID("ecfg_abc123"),
    vercelgc.WithToken(os.Getenv("VERCEL_GC_TOKEN")),
)
```

If no connection is configured at all, `Resolve` returns an error naming
`GLOBAL_CONFIG`, `EDGE_CONFIG`, `WithConnectionString`, and `WithStoreID`/`WithToken`
so the fix is obvious from the error message alone.

### Options

| Option | Effect |
| --- | --- |
| `WithConnectionString(s)` | Set store, token, and host from a full connection string, overriding both `GLOBAL_CONFIG` and `EDGE_CONFIG` |
| `WithStoreID(id)` | Set the store used by refs that name only a key |
| `WithToken(t)` | Set the Global Config read token |
| `WithBaseURL(u)` | Override the API origin, for an `httptest.Server` or a proxy; redirects the host even when a connection string supplies its own, so every request (token included) goes to the named host |
| `WithHTTPClient(c)` | Inject a custom `*http.Client`; a nil client is a no-op |

## No native watch

Vercel exposes no streaming or blocking read for Global Config, so this provider
deliberately does not implement `mamori.WatchableProvider`, and mamori wraps it in
the polling adapter instead.

Each `Resolve` requests the store's `/digest` endpoint, which Vercel replaces on
any edit to any key, and refetches `/items` (the full item body) only when that
hash moved. This makes an unchanged poll tick cost one small request per field and
zero body fetches; only a tick that follows a real edit pays for the larger
`/items` fetch. `ResolveBatch` groups refs by store, so a Load-time read of many
keys in one store costs one digest request and one items request total, not one
pair per key.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake HTTP transport (`go test ./...`) |
| Value mapping (string, boolean, number, null, object/array), `#field` and `#/json/pointer` selection | **Verified** (unit tests) |
| Digest-gated fetching: an unchanged store never refetches `/items`; an edit triggers exactly one refetch, and the fresh snapshot is actually installed | **Verified** (unit tests) |
| Not-found (unknown store via 404, unknown key via absence from the item map), null-is-a-value vs. not-found | **Verified** (unit tests) |
| Error classification (401/403/429/400/5xx), exercised from both the digest fetch and the items fetch | **Verified** (unit tests + `providertest` `ErrorClassification` case) |
| Batch resolution grouped by store, omitting not-found refs per the `BatchProvider` contract | **Verified** (unit tests) |
| Two stores holding independent snapshots; concurrent resolve across stores | **Verified** (unit tests, including `-race`) |
| Context cancellation | **Verified** (unit tests) |
| `WatchableProvider` is deliberately not implemented | **Verified** (`TestProviderIsNotWatchable`) |
| End-to-end against a real Global Config store | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that emulates the Global
Config read API (`/digest` and `/items`, including injectable per-status
failures), so `go test ./...` requires **no** network access and **no** Vercel
credentials.

### Live integration test

An integration test exercises a real Global Config store. It is guarded by a build
tag and skips unless `GLOBAL_CONFIG` (or `EDGE_CONFIG`) and `VERCEL_GC_TEST_KEY`
(the key of an existing entry) are set. It cannot create keys - that would need
Vercel's management API - so it verifies the read path, and is also the only way
to confirm which shape the `/digest` endpoint actually returns in production: a
bare JSON string or an object carrying a `digest` field, since Vercel's docs
describe the response as JSON without pinning the shape:

```sh
export GLOBAL_CONFIG='https://global-config.vercel.com/ecfg_xxx?token=yyy'
export VERCEL_GC_TEST_KEY=my-existing-key
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/vercel-gc
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
