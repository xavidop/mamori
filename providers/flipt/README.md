# mamori Flipt provider

[Flipt](https://www.flipt.io) feature-flag provider for [mamori](https://github.com/xavidop/mamori). Evaluates flags with the official [Flipt Go evaluation SDK](https://pkg.go.dev/go.flipt.io/flipt/sdk/go) over the HTTP transport.

```bash
go get github.com/xavidop/mamori/providers/flipt
```

```go
import _ "github.com/xavidop/mamori/providers/flipt" // registers flipt://
```

## Scheme

```
flipt://<namespace>/<flag-key>[#attachment][?entity=<id>]
```

- `namespace` and `flag-key` are the two path segments, both required.
- The optional `?entity=<id>` sets the evaluation entity id used for percentage rollouts and segment targeting. It defaults to `mamori` when unset.
- The only recognized fragment is `#attachment` (variant flags only, see below). Any other fragment is rejected.

### Boolean flags

A boolean flag resolves to the string `"true"` or `"false"`:

```go
type Config struct {
    NewCheckout bool `source:"flipt://production/new-checkout"`
}
```

### Variant flags

A variant flag resolves to the matched variant key:

```go
type Config struct {
    PlanTier string `source:"flipt://production/plan-tier?entity=user-42"`
}
```

Add the `#attachment` fragment to resolve to the variant's attachment instead (a JSON string, ready for mamori's JSON decoding):

```go
type Config struct {
    PlanLimits PlanLimits `source:"flipt://production/plan-tier#attachment"`
}
```

The provider auto-detects the flag kind: it issues a variant evaluation first and, when Flipt reports a type mismatch (the flag is boolean), falls back to a boolean evaluation. A flag that does not exist resolves to `mamori.ErrNotFound`.

Feature-flag values are not secrets, so `Value.Sensitive` is `false` and `Value.Version` is a content hash (`mamori.VersionHash`), which still gives mamori cheap, correct change detection.

## Authentication

The Flipt server address comes from `FLIPT_URL` (default `http://localhost:8080`). An optional client token is read from `FLIPT_TOKEN` and sent as a static bearer credential. Both can be set explicitly:

```go
mamori.WithProvider(flipt.New(
    flipt.WithURL("https://flipt.example.com"),
    flipt.WithToken("<flipt-client-token>"),
))
```

Both `FLIPT_URL` and `FLIPT_TOKEN` are read lazily on first resolve, so the provider is safe to register from `init` even when no configuration is present at process start.

## Watch

Flipt has no native change-notification API for evaluation, so this provider is not watchable - mamori wraps it in its polling adapter automatically (interval + jitter). Configure with `mamori.WithPollInterval`.

## What is verified

- ✅ Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-memory fake of the Flipt evaluation client (injected through a minimal `evaluator` interface), so no network is required. Boolean flags, variant flags, variant attachments, entity selection, and `ErrNotFound` semantics are all covered.
- ⚠️ Live Flipt behavior is exercised by `//go:build integration` tests requiring a reachable Flipt server and a pre-created flag, **not** run in CI by default. See `flipt_integration_test.go` for the required environment variables.

Passes the mamori conformance kit (`SkipWatch: true`). 🛡️
