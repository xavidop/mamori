# Cloudflare Workers KV provider design

**Status:** approved
**Date:** 2026-07-31

Adds `providers/cloudflare-kv`, a provider resolving configuration values from
[Cloudflare Workers KV](https://developers.cloudflare.com/kv/) over its REST
API, using the standard library only.

This is the second module in a stack whose base is
[`providers/vercel-gc`](2026-07-31-vercel-global-config-provider-design.md).
Where that design applies, this one follows it rather than reinventing, and the
differences below are the parts that genuinely differ.

## Why this backend

Workers KV is where teams on Cloudflare keep the switches their edge code reads
at runtime: feature toggles, redirect maps, per-tenant settings. A Go service
running beside a Cloudflare frontend currently has no way to read the same
values its Workers read.

The read API is a plain authenticated HTTPS call, so this module needs
`net/http` and nothing else. Cloudflare publishes a Go SDK; it is deliberately
not used, because pulling a full API client in to perform two GETs would put a
large dependency tree into a user's build for no benefit, which is the exact
thing this repo's module-per-provider layout exists to prevent.

## What this is not

- **Not watchable.** Workers KV exposes no streaming read, no blocking read,
  and no digest or ETag equivalent for change detection. Per
  [writing-a-provider/capabilities](../../../site/src/pages/docs/writing-a-provider/capabilities.md),
  a provider must never fake a `Watch` with an internal ticker, so this
  provider does not implement `WatchableProvider` and mamori drives it with the
  polling adapter.
- **Not a snapshot cache.** vercel-gc holds a per-store snapshot because
  Vercel's `/digest` makes staleness detectable for free. Workers KV has no
  such endpoint, so caching would mean guessing. Every `Resolve` fetches its
  key. Callers wanting coalescing compose `middleware.Cache`, which already
  exists.
- **Not Cloudflare Secrets Store.** That product is deliberately write-only:
  secrets bind to Workers and the API will not read a value back. A provider
  for it is impossible, not merely awkward. The README says so, so nobody
  tries.
- **Not a write path.** mamori is not a store.

## Scheme and ref grammar

```
cloudflare-kv://<key>                      namespace from configuration
cloudflare-kv://<key>?namespace=<id>       explicit namespace
cloudflare-kv://<key>#field                select a field of a JSON-valued key
cloudflare-kv://<key>#/a/b                 RFC 6901 pointer selection
```

```go
type Config struct {
    CheckoutV2 bool   `source:"cloudflare-kv://new-checkout"`
    LogLevel   string `source:"cloudflare-kv://config/log-level" default:"info"`
    Timeout    string `source:"cloudflare-kv://api-config#timeout"`
    Other      string `source:"cloudflare-kv://flag?namespace=b1c2d3..."`
}
```

### Why the namespace is not a path segment

vercel-gc distinguishes `<key>` from `<store>/<key>` by segment count, which is
safe only because Global Config keys cannot contain a slash. **Workers KV keys
can.** They are up to 512 bytes of any printable, non-whitespace characters,
and `config/log-level` is an ordinary key name.

So the entire ref path is the key, always, and a non-default namespace is
selected with the `?namespace=` query option. This is unambiguous for every
legal key, where a segment-count rule would silently misread the common case of
a slash-separated key name into a namespace plus a shorter key.

The key is `url.PathEscape`d when built into the request URL, so keys
containing `:`, `%`, `?`, or `#` survive the round trip. A `#` in a key cannot
be expressed in a ref, because mamori's grammar claims `#` for field selection;
the README states that limit rather than leaving it to be discovered.

## Authentication and configuration

Two values are required: a Cloudflare API token with Workers KV read
permission, and the account ID. A default namespace ID is optional but is what
makes the common ref form usable.

Read lazily at resolve time, so registering from `init` is safe with no
credentials present at process start. Precedence is explicit options, then
environment:

| Setting | Option | Environment |
| --- | --- | --- |
| API token | `WithAPIToken` | `CLOUDFLARE_API_TOKEN` |
| Account ID | `WithAccountID` | `CLOUDFLARE_ACCOUNT_ID` |
| Default namespace | `WithNamespaceID` | `CLOUDFLARE_KV_NAMESPACE_ID` |

Plus `WithBaseURL` for an `httptest.Server` or a proxy, and `WithHTTPClient`.
Following the fix applied to vercel-gc, **no error message may embed a token,
an account ID, or a resolved value**, and the token travels only in an
`Authorization: Bearer` header.

A missing token, account ID, or namespace produces an error naming both the
option and the environment variable that would supply it.

## Resolve

```
GET {base}/accounts/{account}/storage/kv/namespaces/{namespace}/values/{key}
Authorization: Bearer <token>
```

The response body **is the raw stored value**, not a JSON envelope. This is the
one genuinely surprising thing about the API and the source of its asymmetry
with the bulk endpoint below, so it is called out in the code and the README.

- A 404 means the key is absent: return `ErrNotFound` so defaults apply.
- `Value.Bytes` is the body, byte for byte. There is no unwrapping step,
  because Workers KV stores opaque bytes rather than JSON values. `#key`
  selection still applies via `mamori.SelectKey`, which parses the bytes as
  JSON only when a selector is present.
- `Value.Version` is `mamori.VersionHash` of the resolved bytes. Workers KV
  exposes no revision or ETag to this endpoint.
- `Value.Sensitive` is `false`. KV is a general-purpose store and Cloudflare's
  own docs note that anyone with namespace read access sees values in plain
  text, so treating it as a managed secret store would overstate its
  guarantees. Wrapping a field in `secret.String` still redacts it.
- `Metadata` carries the namespace id. Never the value, never the account id.

## ResolveBatch

```
POST {base}/accounts/{account}/storage/kv/namespaces/{namespace}/bulk/get
{"keys": ["a", "b"], "type": "text"}

-> {"success": true, "result": {"values": {"a": "...", "b": "..."}}, "errors": [], "messages": []}
```

Keys absent from the namespace are **omitted from `values`**, which lines up
exactly with the `BatchProvider` contract that a not-found ref be omitted so
mamori applies its default.

Two things this endpoint forces, both of which are requirements rather than
niceties:

1. **A 100-key ceiling per request.** `ResolveBatch` must chunk into groups of
   at most 100 and merge the results. A config with more than 100 refs against
   one namespace is unusual but entirely legal, and silently truncating it
   would drop fields with no error at all.
2. **A JSON envelope, unlike the single-key GET.** The bulk response wraps
   values in `result.values` and reports failure in `success` and `errors`,
   while the single GET returns naked bytes. The two paths therefore need
   separate parsing, and `type: "text"` must be sent so values come back as
   strings rather than being JSON-parsed by Cloudflare.

Refs are grouped by namespace first, since `?namespace=` allows a batch to span
several.

## Error classification

Cloudflare returns a JSON envelope with an `errors` array on failure. The
mapping keys on HTTP status, matching `classifyStatus` in vercel-gc and
`classifyDopplerStatus`:

| Status | Kind |
| --- | --- |
| 401 | `ErrUnauthenticated` |
| 403 | `ErrPermissionDenied` |
| 404 | `ErrNotFound` (absent key on the single GET, or an unknown namespace on either) |
| 429 | `ErrRateLimited` |
| 400 | `ErrInvalid` |
| 5xx | `ErrUnavailable` |
| anything else | unclassified |

Both paths must handle 404 before classification reaches them. This is easy to
get wrong, because `Resolve` and `ResolveBatch` are separate code paths against
differently shaped endpoints: a 404 branch on only one of them makes the two
disagree about the identical condition, which breaks the core's stated
invariant that `ResolveBatch` cuts round trips without changing what a ref
resolves to. On the bulk path the whole namespace is skipped, so its refs take
their defaults rather than failing the batch and taking refs in other
namespaces down with them.

One honest caveat belongs in the code and the README: a 404 from this API means
either an absent key or an absent namespace, and the response envelope does not
reliably distinguish them. Reporting `ErrNotFound` for both is right for the
key case and merely unhelpful for the namespace case, and a misconfigured
namespace therefore presents as every field falling back to its default. The
README says to check `Status()` when every field is unexpectedly defaulted.

A bulk response carrying `success: false` with a 200 status is treated as
`ErrInvalid` wrapping the first error's message, since the envelope is the only
signal available.

## Testing

Mirrors vercel-gc, including the constraint that sank a naive approach there:
`providertest`'s `NoGoroutineLeak` runs `goleak.VerifyNone` with **no ignore
options**, so the fake must be driven through an in-process `RoundTripper` and
never a live `httptest.Server`.

| Aspect | How |
| --- | --- |
| `providertest.Run` conformance | Against an `httptest`-free fake serving both the single and bulk endpoints |
| Key escaping | keys containing `/`, `:`, `%`, and a space round-trip correctly |
| Batch chunking | 250 refs produce exactly 3 bulk requests, and every value is returned |
| Batch omission | absent keys omitted, siblings survive, matching the defect found in vercel-gc's `ResolveBatch` |
| Resolve and ResolveBatch agree | same ref with an absent `#field` behaves identically through both paths |
| Error classification | one case per status, plus `success: false` on a 200, plus the unmapped-status branch |
| Token safety | no error text contains the token or the account id |
| Live backend | build-tagged, skipped unless `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and a test namespace and key are set |

The `Resolve`-versus-`ResolveBatch` agreement test and the token-safety test
both exist because the equivalent defects were found in vercel-gc's final
review. They are regression pins for known-real mistakes, not speculation.

## Documentation

Shipping with the code: `providers/cloudflare-kv/README.md`,
`site/src/pages/docs/providers/cloudflare-kv.md` plus its navigation entry, the
row in the site provider matrix, the root `README.md` table row and install
line, and the `skills/mamori` reference entry.
