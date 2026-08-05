# mamori Doppler provider

[Doppler](https://doppler.com) provider for [mamori](https://github.com/xavidop/mamori). Pure `net/http` - no third-party SDK.

```bash
go get github.com/xavidop/mamori/providers/doppler
```

```go
import _ "github.com/xavidop/mamori/providers/doppler" // registers doppler://
```

## Scheme

```
doppler://<project>/<config>#<SECRET_NAME>
```

```go
type Config struct {
    StripeKey secret.String `source:"doppler://backend/prd#STRIPE_API_KEY"`
}
```

- `project` and `config` are the path segments; the secret name is the `#fragment`.
- Values are marked `Sensitive`. `Value.Version` is a content hash (Doppler has no per-secret revision).

## Authentication

A Doppler service token, via `DOPPLER_TOKEN` or explicitly:

```go
mamori.WithProvider(doppler.New(doppler.WithToken("dp.st....")))
mamori.WithProvider(doppler.New(doppler.WithBaseURL("https://api.doppler.com"), doppler.WithHTTPClient(myClient)))
```

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports
`errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Doppler. It
also returns the HTTP client's idle connections to the pool, but only when
that client's `Transport` is non-nil. `New`'s own default client (unless
overridden with `WithHTTPClient`) leaves `Transport` unset, and Go resolves a
nil `Transport` to the shared `http.DefaultTransport`; releasing idle
connections there would evict connections belonging to unrelated code in the
same process, so `Close` skips it. A client injected with `WithHTTPClient` is
never closed or invalidated either way, only its idle connections may be
released.

## Watch

No native change notification - mamori polls (interval + jitter). Configure with `mamori.WithPollInterval`.

## Error classification

A missing secret is not a status-classified error: a 404 is detected first and mapped directly to `mamori.ErrNotFound`, before the general status switch runs. Every other non-2xx response is classified by HTTP status so `mamori.ErrorKind` can distinguish them:

| HTTP status | mamori kind |
| --- | --- |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

429 is the confirmed case: Doppler's API reference documents rate limiting explicitly, including the response headers that accompany a 429. 401 (a missing or malformed token) is well established from Doppler's token-based auth model and its own community support threads, though the API reference's status-code section only describes the 2xx/4xx/5xx categories in general terms rather than spelling out 401 by name. 403, 400, and 5xx are mapped on ordinary HTTP semantics - defensible mappings, not individually confirmed by Doppler's documentation. Codes not listed above report `unknown` rather than being guessed at.

A raw transport failure (no HTTP response at all, e.g. a dropped connection) is returned unclassified, since it could be a client-side problem as easily as a genuine backend outage.

## What is verified

- ✅ Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-process HTTP fake of the Doppler API (injected `*http.Client`), so no network is required.
- ✅ Error classification: a table test drives every mapped status through `classifyDopplerStatus`, a `Resolve`-level test drives a real 403 response through the fake HTTP server, and the conformance `ErrorClassification` case injects mamori sentinels directly at the `http.RoundTripper` (before the fake's handler ever runs) and confirms they survive `http.Client.Do`'s `*url.Error` wrapping unchanged.
- ⚠️ Live Doppler behavior is exercised by `//go:build integration` tests requiring a real token, **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
