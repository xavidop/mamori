# mamori Flagsmith provider

[Flagsmith](https://www.flagsmith.com) feature-flag and remote-config provider for [mamori](https://github.com/xavidop/mamori). Uses the official [Flagsmith Go SDK](https://github.com/Flagsmith/flagsmith-go-client) (`/v4`) in its default remote-evaluation mode.

```bash
go get github.com/xavidop/mamori/providers/flagsmith
```

```go
import _ "github.com/xavidop/mamori/providers/flagsmith" // registers flagsmith://
```

## Scheme

```
flagsmith://<feature-name>[#enabled]
```

The path is the feature name. The fragment selects what to return:

| Ref | Returns |
| --- | --- |
| `flagsmith://homepage_banner` | the feature's value (`feature_state_value`) as bytes |
| `flagsmith://new_checkout_flow#enabled` | the enabled state, the literal text `true` or `false` |
| `flagsmith://db_config#host` | a field selected from a JSON-object feature value |

```go
type Config struct {
    Banner  string `source:"flagsmith://homepage_banner"`
    NewFlow bool   `source:"flagsmith://new_checkout_flow#enabled"`
    DBHost  string `source:"flagsmith://db_config#host"`
}
```

- `#enabled` is a reserved fragment: it always returns the flag's on/off state, never a JSON field named `enabled`.
- Any other `#fragment` runs the shared `mamori.SelectKey` selection over the value, so JSON-valued remote config works like every other provider.
- Values are configuration, not secrets, so they are **not** marked `Sensitive`.
- `Value.Version` is a content hash (`mamori.VersionHash`); Flagsmith exposes no per-feature revision id over the flags API, and the hash still gives correct change detection.
- A feature that is not present in the environment resolves to `mamori.ErrNotFound`, so mamori applies your default / optional handling.

## Authentication

A Flagsmith environment key, via `FLAGSMITH_ENVIRONMENT_KEY` (read lazily at first resolve) or explicitly:

```go
mamori.WithProvider(flagsmith.New(flagsmith.WithEnvironmentKey("ser....")))
```

Point at a self-hosted Flagsmith API with `WithBaseURL`:

```go
mamori.WithProvider(flagsmith.New(
    flagsmith.WithEnvironmentKey("ser...."),
    flagsmith.WithBaseURL("https://flagsmith.example.com/api/v1/"),
))
```

## Watch / polling

Flagsmith has no native change-notification surface exposed here, so this provider is **not** watchable; mamori wraps it in its polling adapter automatically. Configure the cadence with `mamori.WithPollInterval` (interval + jitter). Each poll performs one flags fetch against the Flagsmith API.

## What is verified

- The unit tests and the [`providertest`](../../providertest) conformance kit run against an in-memory fake of the flag source (injected via an unexported seam), so no network and no environment key are required.
- Live Flagsmith behavior - real API fetch, value vs `#enabled`, and `ErrNotFound` for a missing feature - is exercised by `//go:build integration` tests requiring a real environment key and a pre-created feature. These are **not** run by the default `go test ./...` pass. See the header of `flagsmith_integration_test.go` for the environment variables.

Passes the mamori conformance kit.
