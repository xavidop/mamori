# mamori Split (Harness FME) provider

[Split](https://www.split.io) (now part of Harness FME) feature-flag provider for [mamori](https://github.com/xavidop/mamori). Wraps the official [`go-client`](https://github.com/splitio/go-client) SDK.

```bash
go get github.com/xavidop/mamori/providers/split
```

```go
import _ "github.com/xavidop/mamori/providers/split" // registers split://
```

## Scheme

```
split://<feature-flag-name>[?key=<traffic-key>]
```

```go
type Config struct {
    Checkout string `source:"split://new-checkout"`
    Layout   string `source:"split://homepage-layout?key=user-42"`
}
```

- The path is the feature-flag name. The provider evaluates the flag and returns its **treatment** as a plain string ("on", "off", or any named treatment you configured in Split).
- A flag that Split does not know about - archived, misspelled, or not yet ready - evaluates to Split's special "control" treatment, which the provider maps to `mamori.ErrNotFound` (so defaults / optional fields apply).
- Values are **not** marked `Sensitive` (feature flags carry rollout state, not secrets). `Value.Version` is a content hash of the treatment (Split exposes no per-flag revision through the treatment API), so the version changes exactly when the treatment changes.

## Traffic key

Split evaluates every flag on behalf of a **traffic key** - the identifier of the user, account, or entity the rollout targets. Percentage rollouts and targeting rules are computed from a hash of this key, so the same key always yields the same treatment for a given flag configuration.

The key is resolved as:

1. the ref's `?key=<traffic-key>` option, when present, otherwise
2. the provider's default key (`WithKey`), which defaults to `"mamori"`.

```go
// Per-ref key:
Layout string `source:"split://homepage-layout?key=user-42"`

// Or a process-wide default key:
mamori.WithProvider(split.New(split.WithKey("tenant-7")))
```

## Authentication

A Split server-side SDK key, via `SPLIT_API_KEY` or explicitly:

```go
mamori.WithProvider(split.New(split.WithAPIKey("your-server-side-sdk-key")))
```

The Split client is built lazily on first `Resolve`, so registering the provider never contacts the network and never fails for lack of configuration.

## Polling and readiness

- **Readiness:** the Split SDK downloads flag definitions from Split's servers in a background goroutine after the factory is created; until that first sync completes every evaluation returns "control". The provider therefore blocks on the SDK's `BlockUntilReady` during its lazy construction (bounded by `WithReadyTimeout`, 10s by default). If the client cannot become ready in time - bad SDK key, unreachable backend - the first `Resolve` returns that initialization error rather than masking it as not-found.
- **Watch:** the SDK refreshes flag definitions on its own interval but exposes no clean per-flag push, so this provider is **not** watchable. mamori polls it (interval + jitter); configure with `mamori.WithPollInterval`.

## What is verified

- Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-memory fake Split client (un-seeded flags return "control" -> `ErrNotFound`), so no network is required. Verified here: scheme, default-key and per-ref-key evaluation, control -> `ErrNotFound`, content-hash versioning, context cancellation, missing-SDK-key error, and non-watchability.
- Live Split behavior - real SDK-key auth, `BlockUntilReady` startup, and end-to-end treatment evaluation - is exercised by `//go:build integration` tests requiring a real `SPLIT_API_KEY` and a pre-created flag, and is **not** run in CI by default:

  ```bash
  export SPLIT_API_KEY=your-server-side-sdk-key
  export SPLIT_TEST_FLAG=my-feature-flag
  export SPLIT_TEST_KEY=user-123   # optional; default traffic key is "mamori"
  go test -tags=integration -run TestLive ./...
  ```

Passes the mamori conformance kit.
