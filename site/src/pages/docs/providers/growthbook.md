---
layout: ../../../layouts/DocsLayout.astro
title: GrowthBook provider
---

# GrowthBook

Load a [GrowthBook](https://www.growthbook.io) feature's evaluated value as config.

| | |
| --- | --- |
| Scheme | `growthbook://` |
| Module | `github.com/xavidop/mamori/providers/growthbook` |
| Sensitive | no |
| Watch | poll |
| Auth | GrowthBook client key + API host |

## Install

```bash
go get github.com/xavidop/mamori/providers/growthbook
```

```go
import _ "github.com/xavidop/mamori/providers/growthbook"
```

## Using the ref

A `growthbook://` ref points at one feature key. It resolves to that feature's evaluated value.

```text
growthbook://<feature-key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<feature-key>` | yes | The GrowthBook feature key. |
| `#json-key` | no | Select one field from a JSON feature value (via SelectKey). |

The value maps to bytes by type (boolean, string, number, or JSON). A feature not present in the loaded feature set resolves to not-found.

**Examples**

- `growthbook://dark-mode` resolves to `true` / `false`.
- `growthbook://checkout-config#timeout_ms` resolves to the `timeout_ms` field of a JSON feature value.

## Watch

GrowthBook loads its features from the API and refreshes them; mamori polls (`WithPollInterval` + jitter).

## Configuration

```go
import gbprov "github.com/xavidop/mamori/providers/growthbook"

mamori.WithProvider(gbprov.New(
	gbprov.WithClientKey(os.Getenv("GROWTHBOOK_CLIENT_KEY")),
	gbprov.WithAPIHost("https://cdn.growthbook.io"),
))
```

Verified with an injected fake (un-seeded features resolve to not-found); live behavior is covered by `//go:build integration` tests.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting GrowthBook. It closes the underlying GrowthBook SDK client, which stops its plugins and, for API-backed operation, the background feature-refresh polling. This provider has no option to inject a pre-built SDK client, so the client `Close` releases is always its own; an `*http.Client` supplied with `WithHTTPClient` only customizes the transport the SDK uses internally and is not itself closed.

## Error classification

Beyond not-found, this provider has no error vocabulary to classify against - a non-not-found evaluator error reports `unknown`. The chain is preserved: `Resolve` wraps the error with `%w`, so `errors.Is` / `errors.As` still reach it.
