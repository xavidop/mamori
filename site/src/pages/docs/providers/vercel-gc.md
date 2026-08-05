---
layout: ../../../layouts/DocsLayout.astro
title: Vercel Global Config
---

# Vercel Global Config

Load a value from [Vercel Global Config](https://vercel.com/docs/global-config) (formerly Edge Config), Vercel's globally replicated key-value store for feature flags, redirect maps, and experimentation settings. Pure `net/http`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `vercel-gc://` |
| Module | `github.com/xavidop/mamori/providers/vercel-gc` |
| Sensitive | no |
| Watch | poll (digest) |
| Auth | `GLOBAL_CONFIG` (or legacy `EDGE_CONFIG`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/vercel-gc
```

```go
import _ "github.com/xavidop/mamori/providers/vercel-gc"
```

## Using the ref

A `vercel-gc://` ref points at one key in a Global Config store, optionally selecting a field from a JSON value stored there.

```text
vercel-gc://<key>
vercel-gc://<store-id>/<key>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<store-id>` | no | The Global Config store id. Omit it to use the store named by `GLOBAL_CONFIG`/`EDGE_CONFIG`; a two-segment path names it explicitly. |
| `<key>` | yes | The Global Config key. A key can never contain a slash, so one path segment is always a key and two segments are `<store-id>/<key>`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued key via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `vercel-gc://log-level` reads key `log-level` from the store named by `GLOBAL_CONFIG`/`EDGE_CONFIG`.
- `vercel-gc://ecfg_abc123/log-level` reads the same key from an explicit store.
- `vercel-gc://api-config#timeout` reads field `timeout` of the JSON-valued key `api-config`.
- `vercel-gc://api-config#/retry/maxAttempts` reads a nested field by JSON Pointer.

```go
type Config struct {
	LogLevel   string `source:"vercel-gc://log-level"`
	MaxRetries int    `source:"vercel-gc://max-retries"`
	APITimeout string `source:"vercel-gc://api-config#timeout"`
}
```

A stored JSON string is unwrapped to its raw text; every other JSON type (boolean, number, object, array) passes through as its own compacted JSON encoding, so a number is exactly what the store holds rather than what a float64 round trip would produce - a deliberate difference from the LaunchDarkly provider, which evaluates flags through an SDK value type backed by `float64`. A key stored as `null` exists and resolves to the four bytes `null`; only an absent key is not-found. `Value.Sensitive` is always `false` (Global Config holds flags and redirects, not managed secrets - wrap a field in `secret.String` for redaction anyway), and `Value.Version` is a content hash rather than the store's digest, because the digest moves on every edit to any key in the store.

## Error classification

A missing store is detected as a 404 before status classification runs, and reports `not_found` directly. A missing key in a store that does exist is detected by its absence from the fetched items, not a status code, and also reports `not_found`. Beyond those two cases, failures are classified by HTTP status:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 429 | `rate_limited` |
| 400 | `invalid` |
| 5xx | `unavailable` |
| anything else | `unknown` |

A 403 is not always an insufficient credential: Vercel's documented error body for a request missing a token also carries `"code": "forbidden"`, and Vercel has not published its full error-code vocabulary, so the mapping keys on status rather than guessing at a body it cannot rely on.

## Watch

Vercel exposes no streaming or blocking read for Global Config, so this provider does not implement `WatchableProvider` - mamori polls it instead (`WithPollInterval` + jitter). Each `Resolve` requests the store's cheap `/digest` endpoint and refetches the full `/items` body only when the digest moved, so an unchanged poll tick costs one small request per field and no body fetch.

## Configuration

```go
import vercelgc "github.com/xavidop/mamori/providers/vercel-gc"

mamori.WithProvider(vercelgc.New(vercelgc.WithConnectionString(os.Getenv("GLOBAL_CONFIG"))))
```

`GLOBAL_CONFIG` (falling back to `EDGE_CONFIG` for stores connected before Vercel's rename) is read lazily at resolve time, so a blank import alone is enough once the environment variable is set. The token always travels as an `Authorization: Bearer` header, never a URL query parameter, so a request to the Global Config API cannot leak it into a log line, a trace span, or an error message. That guarantee is about requests, not about parsing the connection string itself: a malformed connection string (a trailing newline is the realistic case) can otherwise surface inside a `url.Parse` diagnostic, so this provider strips the URL from that error before returning it. Passing a full connection string to `WithBaseURL` by mistake is the other way the token can end up in a transport error, since it redirects every request to the host it names.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Vercel. It also returns the HTTP client's idle connections to the pool, but only when that client's `Transport` is non-nil. `New`'s own default client (unless overridden with `WithHTTPClient`) leaves `Transport` unset, and Go resolves a nil `Transport` to the shared `http.DefaultTransport`; releasing idle connections there would evict connections belonging to unrelated code in the same process, so `Close` skips it. A client injected with `WithHTTPClient` is never closed or invalidated either way, only its idle connections may be released.

Verified with an in-memory fake HTTP transport (digest-gated fetching, value mapping, error classification, batch resolution, concurrent resolve), so the conformance kit runs without a live Vercel store. A `//go:build integration` test exercises a real store when `GLOBAL_CONFIG`/`EDGE_CONFIG` and `VERCEL_GC_TEST_KEY` are set.
