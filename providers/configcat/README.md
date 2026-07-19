# mamori ConfigCat provider

[ConfigCat](https://configcat.com) feature-flag provider for [mamori](https://github.com/xavidop/mamori). Backed by the official [`configcat/go-sdk`](https://github.com/configcat/go-sdk).

```bash
go get github.com/xavidop/mamori/providers/configcat
```

```go
import _ "github.com/xavidop/mamori/providers/configcat" // registers configcat://
```

## Scheme

```
configcat://<setting-key>
```

The whole ref is the setting key as defined in the ConfigCat dashboard. The resolved value is the evaluated setting rendered as text:

```go
type Config struct {
    AwesomeEnabled bool   `source:"configcat://isAwesomeFeatureEnabled"`
    Greeting       string `source:"configcat://welcomeMessage"`
    SamplingRatio  string `source:"configcat://samplingRatio"`
}
```

Value rendering:

| ConfigCat type | Resolved bytes            |
| -------------- | ------------------------- |
| Bool           | `"true"` / `"false"`      |
| String         | the raw string            |
| Int            | decimal form, e.g. `"42"` |
| Double         | decimal form, e.g. `"3.14"` |

- A setting key that is **not present** in the config resolves to `mamori.ErrNotFound`. The SDK's default value is never substituted for a missing key, so defaults and optional fields behave correctly.
- Feature flags are configuration, not secrets, so `Value.Sensitive` is `false`.
- ConfigCat exposes no stable per-setting revision, so `Value.Version` is a content hash (`mamori.VersionHash`), which still gives cheap, correct change detection.
- If a string setting holds a JSON object you may select a field with the shared `#key` convention, e.g. `configcat://payload#level`.

## Authentication

A ConfigCat SDK key, via `CONFIGCAT_SDK_KEY` or explicitly:

```go
// Ambient: reads CONFIGCAT_SDK_KEY lazily when the client is first built.
import _ "github.com/xavidop/mamori/providers/configcat"

// Explicit configuration:
mamori.WithProvider(configcat.New(configcat.WithSDKKey("configcat-sdk-1/...")))

// Tune the SDK's background refresh cadence:
mamori.WithProvider(configcat.New(
    configcat.WithSDKKey("configcat-sdk-1/..."),
    configcat.WithPollInterval(30*time.Second),
))
```

## Polling / Watch

ConfigCat has no push-style change notification, so this provider is **not** watchable and mamori polls it. There are two independent cadences:

- The ConfigCat SDK auto-polls the CDN in the background (default 60s, tune with `configcat.WithPollInterval`). This keeps the in-memory config fresh.
- mamori re-resolves your struct on its own schedule (tune with `mamori.WithPollInterval`). This is what pushes a changed flag into your config value.

## What is verified

- Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-memory fake of the SDK (injected through a minimal `keys` / `value` client interface), so no network is required. Verified: scheme, value rendering for bool/string/number, not-found via the config key set, versioning, context cancellation, concurrency, and goroutine hygiene.
- Live ConfigCat behavior (real SDK key, real CDN, background auto-poll) is exercised by the `//go:build integration` test in `configcat_integration_test.go`, which needs a real SDK key and is **not** run by `go test ./...` or in CI by default. Run it with:

  ```bash
  export CONFIGCAT_SDK_KEY=configcat-sdk-1/xxxx/yyyy
  export CONFIGCAT_TEST_SETTING=isPOCFeatureEnabled   # a key that exists
  go test -tags=integration -run TestLive ./...
  ```

Passes the mamori conformance kit.
