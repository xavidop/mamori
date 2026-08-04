---
layout: ../../../layouts/DocsLayout.astro
title: Cloudflare Workers KV
---

# Cloudflare Workers KV

Load a value from [Cloudflare Workers KV](https://developers.cloudflare.com/kv/), Cloudflare's low-latency, eventually-consistent key-value store replicated to its edge network. Pure `net/http`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `cloudflare-kv://` |
| Module | `github.com/xavidop/mamori/providers/cloudflare-kv` |
| Sensitive | no |
| Watch | poll |
| Auth | `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_KV_NAMESPACE_ID` |

## Install

```bash
go get github.com/xavidop/mamori/providers/cloudflare-kv
```

```go
import _ "github.com/xavidop/mamori/providers/cloudflare-kv"
```

## Using the ref

A `cloudflare-kv://` ref points at one key in a Workers KV namespace, optionally selecting a field from a JSON value stored there.

```text
cloudflare-kv://<key>
cloudflare-kv://<key>?namespace=<id>
cloudflare-kv://<key>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<key>` | yes | The **entire ref path**, including any slashes it contains. Workers KV keys are up to 512 bytes of any printable, non-whitespace character, so `config/prod/log-level` is one ordinary key, not a namespace plus a shorter key - that is why the namespace is never taken from the path. |
| `?namespace=<id>` | no | Overrides the configured namespace for this ref only. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued key via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `cloudflare-kv://log-level` reads key `log-level` from the configured namespace.
- `cloudflare-kv://config/prod/log-level` reads the single key `config/prod/log-level` - the slashes are part of the key.
- `cloudflare-kv://log-level?namespace=abcd1234` reads the same key from an explicit namespace.
- `cloudflare-kv://api-config#timeout` reads field `timeout` of the JSON-valued key `api-config`.

```go
type Config struct {
	LogLevel   string `source:"cloudflare-kv://log-level"`
	Region     string `source:"cloudflare-kv://region?namespace=abcd1234"`
	APITimeout string `source:"cloudflare-kv://api-config#timeout"`
}
```

A key containing a literal `#` cannot be expressed as a ref: mamori's grammar parses the `#key` fragment before the `?opts` query, claiming `#` for field selection, so there is no way to escape one into a path.

`Value.Sensitive` is always `false` - Cloudflare's own docs note that anyone with read access to a namespace sees its values in plain text, so this provider does not call Workers KV a managed secret store. Wrap a field in `secret.String` for redaction anyway. `Value.Version` is a content hash rather than a Cloudflare-issued revision, because Workers KV exposes no revision number or ETag for a key.

## Fetching: single-key GET vs. bulk POST

A single ref resolves through the single-key value endpoint, whose response body is the value's **raw stored bytes** - no envelope. `mamori.Load` instead calls `ResolveBatch`, which groups refs by namespace and hits the bulk endpoint in chunks of at most 100 keys (the API's documented ceiling): its response wraps every value inside a JSON envelope, `{"result":{"values":{...}}}`. This asymmetry is real: a single-key read is already unwrapped; a bulk read is not. A key absent from a bulk response is simply omitted from the result, so mamori applies that field's default rather than failing the whole batch.

## Error classification

A 404 is detected before status classification runs, on both the single-key GET path and the bulk POST path - but the two do different things with it. `Resolve` returns it directly as an error satisfying `mamori.ErrNotFound`. `ResolveBatch` does not return an error for it at all: the bulk endpoint has no per-key 404 (a missing key is simply omitted from a successful response's `values`), so a 404 there can only mean the namespace itself does not exist, and `ResolveBatch` treats that exactly like a namespace full of absent keys - omitting every requested key from the result so mamori applies each ref's default, rather than failing the whole call and losing every sibling ref over one bad namespace.

Beyond 404, every other failure is classified by HTTP status, identically on both paths:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 408, 429 | `rate_limited` |
| 400, 422 | `invalid` |
| 5xx, and any other status not named above | `unavailable` |

The mapping is `httpcore.ClassifyStatus`, shared with every other `httpcore`-backed provider. An unrecognized status reports `unavailable` (transient, so mamori backs off and retries) rather than `unknown`.

An absent key and an absent namespace both return a plain 404 and are indistinguishable in the response. A namespace id that is simply wrong therefore presents exactly like a namespace full of genuinely absent keys: every field in your config silently falls back to its default, on either the single-key or the batch path. If everything unexpectedly defaults at once, check `Status()` before assuming the keys themselves are missing.

The API token always travels as an `Authorization: Bearer` header, never a query parameter. The account id and namespace id are part of every request URL, and `http.Client.Do` wraps transport failures in a `*url.Error` whose message renders that URL - this provider strips it before returning the error, so an ordinary network hiccup never puts either id into an error message. Stripping the namespace id from errors does not conflict with this provider publishing it in `Value.Metadata["namespace"]` on every resolved value: a namespace id is an identifier, not a credential, and the stripping is about not rendering whole request URLs, not about the id being secret.

## Watch

The Workers KV REST API exposes no streaming or blocking read, and no digest or ETag to gate a cache on. So unlike `providers/vercel-gc`, which holds a snapshot behind a cheap digest check, this provider holds no snapshot at all - every `Resolve` is a live fetch. This provider does not implement `WatchableProvider`; mamori polls it instead (`WithPollInterval` + jitter), using the content-hash `Version` to detect a change between ticks. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads across a poll interval.

## Cloudflare Secrets Store is not supported

[Secrets Store](https://developers.cloudflare.com/secrets-store/) is a distinct Cloudflare product, and this provider cannot read from it: Secrets Store values are write-only by design, bound to a Worker at deploy time, with no API endpoint that reads a stored secret back out. There is no HTTPS read path to call, so this is a permanent limitation of the backend, not a gap in this provider.

## Configuration

```go
import cloudflarekv "github.com/xavidop/mamori/providers/cloudflare-kv"

mamori.WithProvider(cloudflarekv.New(
	cloudflarekv.WithAPIToken(os.Getenv("CLOUDFLARE_API_TOKEN")),
	cloudflarekv.WithAccountID(os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
	cloudflarekv.WithNamespaceID(os.Getenv("CLOUDFLARE_KV_NAMESPACE_ID")),
))
```

Reading a key requires an API token, an account id, and a namespace id. Each may be set explicitly (`WithAPIToken`, `WithAccountID`, `WithNamespaceID`) or read from the environment (`CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_KV_NAMESPACE_ID`), read lazily at resolve time so a blank import alone is enough once the environment is set. An explicit option wins over its environment variable; the namespace has one more source - a ref's `?namespace=` option - which wins over both.

Verified with an in-memory fake HTTP transport (value shape, namespace precedence, batch chunking and grouping, error classification, credential hygiene), so the conformance kit runs without a live Cloudflare account. A `//go:build integration` test exercises a real namespace when `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_KV_NAMESPACE_ID`, and `CLOUDFLARE_KV_TEST_KEY` are set.
