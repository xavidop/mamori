# Vercel Global Config provider design

**Status:** approved
**Date:** 2026-07-31

Adds `providers/vercel-gc`, a provider that resolves configuration values from
[Vercel Global Config](https://vercel.com/docs/global-config), the globally
replicated key-value store Vercel applications read at runtime.

The module depends on `net/http` and the standard library only. No Vercel SDK
exists for Go, and none is needed: the read path is a documented HTTPS API.

## Why this backend

Global Config is where Vercel keeps the data an application reads at runtime
without redeploying: feature flags, redirect maps, IP blocklists, and
experimentation settings. It is also the store the Flags SDK's Global Config
adapter reads flag values from, and the store LaunchDarkly and Statsig sync
their flag definitions into when integrated with Vercel. A Go service running
beside a Vercel frontend currently has no way to read the same switches its
frontend reads.

The read API is unusually well suited to mamori:

| Endpoint | Use |
| --- | --- |
| `GET /<store>/digest` | a hash of the whole store, replaced on every update |
| `GET /<store>/items` | every value in one round trip, backing both `Resolve` and `ResolveBatch` |
| `GET /<store>/item/<key>` | not used, see below |

The digest turns each poll into one small request that fetches nothing unless
the store actually changed.

`/item/<key>` is deliberately unused. It looks like the natural fit for a
single `Resolve`, but it costs a full request per key per tick with no way to
tell whether anything changed. Pairing `/digest` with `/items` costs one small
request per resolve and one shared body fetch per actual change, and the whole
store is bounded at a few hundred kilobytes, so fetching all of it is cheaper
than fetching one key repeatedly.

## What this is not

- **Not watchable.** Vercel exposes no streaming or blocking read for Global
  Config. Per [writing-a-provider/capabilities](../../../site/src/pages/docs/writing-a-provider/capabilities.md),
  a provider must never fake a `Watch` with an internal ticker, so this
  provider does not implement `WatchableProvider`. mamori's polling adapter
  drives it, and the digest makes each tick cheap.
- **Not a write path.** Writing to Global Config requires a Vercel REST API
  token rather than a read token, and mamori is not a store.
- **Not Vercel Flags.** Vercel's own flag product (`FLAGS` SDK keys,
  `flags.vercel.com`) is a separate service whose wire protocol is not
  published. It is deliberately out of scope here. See "Deferred" below.

## Scheme and ref grammar

```
vercel-gc://<key>                    store and token from the connection string
vercel-gc://<store-id>/<key>         explicit store
vercel-gc://<key>#field              select a field of a JSON-valued key
vercel-gc://<key>#/a/b               RFC 6901 pointer selection
```

`vercel-gc` follows the vendor-prefixed initialism already used by
`firebase-rc` (remote config), `azure-kv` (key vault), and `aws-sm` (secrets
manager). It leaves `vercel-flags://` and any future Vercel surface a free
name.

```go
type Config struct {
    CheckoutV2 bool   `source:"vercel-gc://new-checkout"`
    LogLevel   string `source:"vercel-gc://log-level" validate:"oneof=debug info warn error"`
    MaxRetries int    `source:"vercel-gc://max-retries" default:"3"`
    APITimeout string `source:"vercel-gc://api-config#timeout"`
    Shared     string `source:"vercel-gc://ecfg_abc123/rollout-stage"`
}
```

### Path parsing

`Ref.Path` is one opaque string, so the store and key are distinguished by
segment count:

| Segments | Meaning |
| --- | --- |
| 1 | key; store comes from the connection string |
| 2 | `<store-id>/<key>` |
| 0, or 3 and up | error at resolve time, naming the ref |

This is unambiguous because Global Config keys are documented as alphanumeric
characters, `_`, and `-` only, up to 256 characters, so a key can never contain
a slash. The provider rejects an empty key and a path with more than two
segments, and otherwise leaves charset validation to the API rather than
duplicating a rule Vercel may loosen.

## Connection string and authentication

Connecting a Global Config store to a Vercel project creates a `GLOBAL_CONFIG`
environment variable holding a connection string:

```
https://global-config.vercel.com/<store-id>?token=<read-token>
```

Stores connected before Vercel renamed Edge Config to Global Config instead
have `EDGE_CONFIG`, pointing at `edge-config.vercel.com`. The provider reads
`GLOBAL_CONFIG` first and falls back to `EDGE_CONFIG`, mirroring what
`@vercel/global-config` does, so a service works either way with no
configuration.

The **host is taken from the connection string**, not hardcoded. That is what
makes the legacy `edge-config.vercel.com` host keep working, and it lets a
self-hosted proxy be pointed at without a new option.

Configuration precedence, resolved lazily at resolve time so registering from
`init` is safe with no credentials present at process start:

1. `WithConnectionString(s)`
2. `WithStoreID(id)` and `WithToken(t)`
3. `GLOBAL_CONFIG`
4. `EDGE_CONFIG`

The token is sent as `Authorization: Bearer <token>` rather than as a `token`
query parameter. Both are documented; the header keeps the credential out of
any URL that might reach a log, a trace span, or an error message.

### Options

| Option | Effect |
| --- | --- |
| `WithConnectionString(s)` | Sets store, token, and host from one string, overriding the environment |
| `WithStoreID(id)` | Sets the default store for one-segment refs |
| `WithToken(t)` | Sets the read token |
| `WithBaseURL(u)` | Overrides the host, for `httptest` or a proxy |
| `WithHTTPClient(c)` | Injects a client for timeouts and transport; nil is a no-op |

A ref naming an explicit store still uses the configured token. Reading two
stores with different tokens means registering two provider instances, which
the `WithProvider` per-call registration already supports.

## Resolve

```
Resolve(ref):
  store, key := parsePath(ref)
  d    := GET {host}/{store}/digest        // always, on every Resolve
  snap := snapshots[store]
  if snap == nil || snap.digest != d:
      items := GET {host}/{store}/items
      snap   = {digest: d, items: items}
      snapshots[store] = snap
  raw, ok := snap.items[key]
  if !ok: return ErrNotFound
  return valueFor(raw, ref)
```

Every `Resolve` performs a network round trip, so there is **no clock and no
TTL anywhere in the provider**. This matters beyond simplicity: `Refresh()` and
`mamori.Doctor` both call `Provider.Resolve` directly, and a time-based cache
would let them return a held value without ever contacting Vercel, quietly
breaking the guarantee each is built to provide.

Under mamori's polling adapter with N `vercel-gc` fields, a tick where nothing
changed costs N digest requests and no item fetches. A tick where the store
changed costs N digest requests and one `/items` fetch. A `Load` costs a single
`/items` request via `ResolveBatch`.

### Concurrency

`snapshots` is a `map[storeID]*snapshot` behind a mutex. The digest request
happens outside the lock; the compare and install happen under it, with a
second check so a goroutine that lost the race installs nothing. Two goroutines
observing the same digest change may both fetch `/items`. That is accepted
rather than prevented: the fetch is an idempotent GET, it happens only on
actual change, and avoiding it would mean either serializing every digest
request or adding a single-flight dependency to a module whose whole appeal is
that it has none.

### ResolveBatch

`ResolveBatch` fetches `/items` once and serves every ref for that store from
it, grouping by store when refs name different ones. Refs whose key is absent
are omitted from the returned map so mamori applies their defaults, per the
`BatchProvider` contract. It also installs the resulting snapshot, so a `Load`
followed by watching costs no redundant fetch.

## Value mapping

`/items` returns a JSON object of key to JSON value. Bytes are produced by
value type, then `#key` selection is applied, matching `valueFor` in
[providers/launchdarkly/launchdarkly.go](../../../providers/launchdarkly/launchdarkly.go)
exactly so the two flag-shaped providers behave identically:

| Stored value | Bytes |
| --- | --- |
| string | the raw text, unquoted |
| boolean | `true` or `false` |
| number | its JSON text, e.g. `5432` or `0.25` |
| object or array | its compacted JSON encoding |
| null | `null` |

Only a string is unwrapped. Every other type passes through as its own
compacted JSON, so a number is exactly what the store holds rather than what a
`float64` round trip would produce. This differs from the LaunchDarkly
provider, which converts a number to its shortest decimal form only because its
SDK hands it a `float64` in the first place.

A key explicitly stored as `null` exists, so it resolves to the four bytes
`null` rather than `ErrNotFound`. Only an absent key is not-found.

Selection order is unwrap first, then `mamori.SelectKey(b, ref.Key)`, then
hash. Selecting a field of a string-valued key therefore fails with
`ErrInvalid` from `SelectKey`, which is correct: there is no field to select.

- `Value.Version` is `mamori.VersionHash` of the resolved bytes. The store
  digest is deliberately **not** used, because it moves whenever any key in the
  store changes and would fire spurious `OnChange` events for every unrelated
  field. The digest goes in `Metadata` alongside the store id.
- `Value.Sensitive` is `false`. Global Config holds flags, redirects, and
  blocklists, not managed secrets. A field wrapped in `secret.String` is still
  redacted, and the README says so.

## Error classification

Beyond not-found, HTTP status maps onto a `mamori.ErrorKind`, following the
structure and the documented-honesty of `classifyDopplerStatus` in
[providers/doppler/doppler.go](../../../providers/doppler/doppler.go):

| Status | Kind |
| --- | --- |
| 401 | `ErrUnauthenticated` |
| 403 | `ErrPermissionDenied` |
| 404 | `ErrNotFound` (store absent; an absent key is detected from `/items`) |
| 429 | `ErrRateLimited` |
| 400 | `ErrInvalid` |
| 5xx | `ErrUnavailable` |
| anything else | unclassified |

One honest caveat belongs in both the code comment and the README: Vercel's
documented error body for a missing authentication token carries
`"code": "forbidden"`, so a 403 from this API can mean an absent credential
rather than an insufficient one. The mapping follows ordinary HTTP semantics
rather than trying to disambiguate from an error vocabulary Vercel has not
published in full.

A missing connection string is an error naming both environment variables and
the explicit options, not a silent empty result.

## Module layout

`providers/vercel-gc` is its own Go module with its own `go.mod`, a `replace`
to the repo root like every other provider, and its own release cadence.
`import _ "github.com/xavidop/mamori/providers/vercel-gc"` registers the scheme
and contacts nothing.

## Testing

| Aspect | How |
| --- | --- |
| `providertest.Run` conformance | Against an `httptest` fake serving `/digest` and `/items` |
| Digest gating | N resolves across an unchanged digest trigger exactly one `/items` fetch; bumping the digest triggers exactly one more. Asserted by counting fake requests |
| Value mapping | string, bool, number, object, array, null, plus `#field` and `#/pointer` selection |
| Not-found | absent key returns `errors.Is(err, mamori.ErrNotFound)`; a null-valued key does not |
| Error classification | one case per status in the table, through `providertest`'s `ErrorClassification` case |
| Connection string | `GLOBAL_CONFIG` parsing, `EDGE_CONFIG` fallback, precedence over both, malformed string, missing entirely |
| Multi-store | two stores hold independent snapshots and digests |
| Concurrency | `-race` with concurrent resolves across two stores |
| Live backend | build-tagged integration test, skipped unless `GLOBAL_CONFIG` and a test key are set |

Unit and conformance tests require no Vercel account and no network.

## Documentation

Shipping with the code, not after it:

- `providers/vercel-gc/README.md`, following the LaunchDarkly README's
  structure: scheme, ref examples, value mapping table, error classification,
  auth, options, testing status, development commands.
- `site/src/pages/docs/providers/vercel-gc.md`, plus its navigation entry.
- Root `README.md`: a row in the provider table
  (`providers/vercel-gc` | `vercel-gc://` | poll (digest) | yes) and the
  module in the install list.
- `skills/mamori`: the scheme added to the provider list it teaches.

## Deferred

Two Vercel surfaces were investigated and consciously left out of this spec:

- **`x/vercelflags`**, an `http.Handler` for the documented
  `/.well-known/vercel/flags` discovery endpoint, which would make a Go
  service's fields visible in the Vercel Flags Explorer. Its `verifyAccess`
  check is JWE `dir` + `A256GCM` keyed on a base64url-decoded `FLAGS_SECRET`
  with payload `{"pur":"proof"}`, implementable with `crypto/aes` alone. Its
  definitions carry no values by construction, which suits mamori's
  metadata-only reporting rule. Worth revisiting once this provider has users.
- **`providers/vercel-flags`**, reading Vercel's own flag service at
  `flags.vercel.com/v1/datafile` with a `/v1/stream` NDJSON native watch. The
  protocol is not published, so it would be the only module in the repo not
  built on a documented contract.

The `vercel-flag-overrides` cookie is out of scope permanently, not merely
deferred: overrides are per-request, and `Get()` returns one snapshot for the
whole process.
