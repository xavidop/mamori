---
layout: ../../../layouts/DocsLayout.astro
title: Doppler provider
---

# Doppler

[Doppler](https://doppler.com) secrets over the REST API. Pure `net/http`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `doppler://` |
| Module | `github.com/xavidop/mamori/providers/doppler` |
| Sensitive | yes |
| Watch | poll |
| Auth | `DOPPLER_TOKEN` (service token) |

## Install

```bash
go get github.com/xavidop/mamori/providers/doppler
```

```go
import _ "github.com/xavidop/mamori/providers/doppler"
```

## Using the ref

A `doppler://` ref points at one secret inside a Doppler project and config. The `#` fragment naming the secret is required.

```text
doppler://<project>/<config>#<SECRET_NAME>
```

| Part | Required | What it means |
| --- | --- | --- |
| `<project>` | yes | The Doppler project. |
| `<config>` | yes | The config (environment) within that project, e.g. `prd`. |
| `#<SECRET_NAME>` | yes | The secret to fetch. Unlike other providers, this fragment is required - a ref with no `#` is an error. |

**Examples**

- `doppler://backend/prd#STRIPE_API_KEY` reads `STRIPE_API_KEY` from the `prd` config of the `backend` project.
- `doppler://backend/prd#DATABASE_URL` reads `DATABASE_URL` from the same config.

```go
type Config struct {
	StripeKey secret.String `source:"doppler://backend/prd#STRIPE_API_KEY"`
	DBURL     secret.String `source:"doppler://backend/prd#DATABASE_URL"`
}
```

Values are marked `Sensitive`. Doppler exposes no per-secret revision, so `Value.Version` is a content hash, which still gives cheap, correct change detection. The provider returns the computed value (with Doppler references resolved), falling back to the raw value.

## Explicit configuration

```go
import dopplerprov "github.com/xavidop/mamori/providers/doppler"

mamori.WithProvider(dopplerprov.New(
	dopplerprov.WithToken(os.Getenv("DOPPLER_TOKEN")),
))
```

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Doppler. It also returns its own idle HTTP connections to the pool, and leaves connections belonging to the rest of your process alone. A client injected with `WithHTTPClient` is never closed, so it stays usable for whatever else holds it.

## Watch

Doppler has no push channel, so mamori polls (`WithPollInterval` + jitter).

## Error classification

A missing secret is detected before any status classification runs: a 404 maps directly to `mamori.ErrNotFound`. Every other non-2xx response is classified by HTTP status:

| HTTP status | mamori kind |
| --- | --- |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

429 is the confirmed case: Doppler's API reference documents rate limiting explicitly, including the response headers that accompany a 429. 401 is well established from Doppler's token-based auth model, though the API reference's status-code section only describes the 2xx/4xx/5xx categories in general terms rather than naming 401 specifically. 403, 400, and 5xx are mapped on ordinary HTTP semantics - defensible, not individually confirmed by Doppler's documentation. A raw transport failure (no HTTP response at all) is returned unclassified, since it could be a client-side problem as easily as a genuine backend outage.

Verified by unit tests and the conformance kit against an in-process HTTP fake of the Doppler API (injected `*http.Client`), so no network is required. The conformance `ErrorClassification` case injects mamori sentinels directly at the `http.RoundTripper`, before the fake's handler runs, and confirms they survive `http.Client.Do`'s error wrapping unchanged. Live behavior is covered by `//go:build integration` tests.
