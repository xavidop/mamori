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

## Watch

No native change notification - mamori polls (interval + jitter). Configure with `mamori.WithPollInterval`.

## What is verified

- ✅ Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-process HTTP fake of the Doppler API (injected `*http.Client`), so no network is required.
- ⚠️ Live Doppler behavior is exercised by `//go:build integration` tests requiring a real token, **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
