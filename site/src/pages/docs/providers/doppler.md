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

## Watch

Doppler has no push channel, so mamori polls (`WithPollInterval` + jitter).

Verified by unit tests and the conformance kit against an in-process HTTP fake of the Doppler API (injected `*http.Client`), so no network is required. Live behavior is covered by `//go:build integration` tests.
