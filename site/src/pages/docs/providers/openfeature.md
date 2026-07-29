---
layout: ../../../layouts/DocsLayout.astro
title: OpenFeature provider
---

# OpenFeature

Load a flag's evaluated value as config through [OpenFeature](https://openfeature.dev), the vendor-neutral feature-flag standard - whatever backend (LaunchDarkly, Flagsmith, GO Feature Flag, your own) your application has installed with `openfeature.SetProvider` is what this provider evaluates against.

| | |
| --- | --- |
| Scheme | `openfeature://` |
| Module | `github.com/xavidop/mamori/providers/openfeature` |
| Sensitive | no |
| Watch | poll |
| Auth | none - evaluates through your application's own installed `openfeature.FeatureProvider` |

## Install

```bash
go get github.com/xavidop/mamori/providers/openfeature
```

```go
import _ "github.com/xavidop/mamori/providers/openfeature"
```

## Using the ref

A `openfeature://` ref points at one flag key.

```text
openfeature://<flag-key>[#json-key][?type=bool|string|int|float|object]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<flag-key>` | yes | The flag key, exactly as your OpenFeature provider expects it. |
| `#json-key` | no | Select one field from an object-valued flag (via `mamori.SelectKey`). |
| `?type=` | no | Pin the evaluation method: `bool`, `string`, `int`, `float`, or `object`. |

OpenFeature evaluates a flag *by type* - there is no single "evaluate and tell me what it is" call. Pin `?type=` in production: it costs exactly one evaluation. Without it, the provider tries `object`, then `bool`, then `string`, stopping at the first that is not a type mismatch - useful while exploring, but up to three evaluations per resolve against your vendor. A flag that cannot be evaluated as any of the three reports an error, not not-found: the flag exists, so mamori must not silently fall back to a `default:`.

**Examples**

- `openfeature://new-checkout?type=bool` on a boolean flag resolves to `true` or `false`.
- `openfeature://max-retries?type=int` on a numeric flag resolves to its decimal form.
- `openfeature://limits#/upload/maxMB?type=object` selects one field out of an object-valued flag.

```go
type Config struct {
	NewCheckout bool   `source:"openfeature://new-checkout?type=bool"`
	MaxUploadMB int    `source:"openfeature://limits#/upload/maxMB?type=object"`
}
```

## Targeting key and identity

Evaluation uses one fixed evaluation context for the whole process: a targeting key (default `"mamori"`, override with `WithTargetingKey`) plus any static attributes from `WithAttributes`. **This is not per-user targeting** - a mamori field holds one value for the whole process, not one per request. Targeting that varies per end user belongs in your application's own OpenFeature calls, not in a config field.

## Error classification

Beyond not-found, `Resolve` classifies the [OpenFeature error code](https://openfeature.dev/specification/types#error-code) carried on the evaluation result:

| OpenFeature error code | mamori kind |
| --- | --- |
| `FLAG_NOT_FOUND` | `not_found` |
| `TYPE_MISMATCH`, `PARSE_ERROR`, `INVALID_CONTEXT`, `TARGETING_KEY_MISSING` | `invalid` |
| `PROVIDER_NOT_READY`, `PROVIDER_FATAL` | `unavailable` |
| `GENERAL` | `unknown` (deliberately unmapped - OpenFeature's catch-all, no reliable cause) |

## Watch

This provider is **not watchable**: OpenFeature has no per-flag change signal exposed uniformly enough at the client level to subscribe to across vendors, so mamori polls it (`WithPollInterval` + jitter).

## Configuration

```go
import (
	"github.com/open-feature/go-sdk/openfeature"
	ofprov "github.com/xavidop/mamori/providers/openfeature"
)

openfeature.SetProvider(myVendorProvider) // your application's own OpenFeature wiring

mamori.WithProvider(ofprov.New(
	ofprov.WithTargetingKey("checkout-service"),
	ofprov.WithAttributes(map[string]any{"region": "eu-west-1"}),
))
```

Verified with an injected fake `openfeature.IClient` (fallback-chain order and stopping, all eight OpenFeature error codes, targeting key/attributes, and the full `providertest` conformance suite). There is no vendor-specific integration test: this provider evaluates through whatever `FeatureProvider` your own application installs, so exercising it against a real backend is a choice made in your application, not here.
