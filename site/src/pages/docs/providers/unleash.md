---
layout: ../../../layouts/DocsLayout.astro
title: Unleash provider
---

# Unleash

Load an [Unleash](https://www.getunleash.io) feature toggle's state (or a variant) as config.

| | |
| --- | --- |
| Scheme | `unleash://` |
| Module | `github.com/xavidop/mamori/providers/unleash` |
| Sensitive | no |
| Watch | poll |
| Auth | `UNLEASH_URL` + `UNLEASH_API_TOKEN` + app name |

## Install

```bash
go get github.com/xavidop/mamori/providers/unleash
```

```go
import _ "github.com/xavidop/mamori/providers/unleash"
```

## Using the ref

A `unleash://` ref points at one feature toggle. Without a fragment it resolves to the toggle's enabled state; a fragment selects a variant instead.

```text
unleash://<feature-toggle>[#variant | #payload]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<feature-toggle>` | yes | The Unleash feature toggle name. |
| `#variant` | no | Resolve to the matched variant's name instead of the on/off state. |
| `#payload` | no | Resolve to the matched variant's payload value. |

A toggle that does not exist resolves to not-found.

**Examples**

- `unleash://new-dashboard` resolves to `true` / `false` - pair it with a `bool` field.
- `unleash://checkout-experiment#variant` resolves to the assigned variant name, e.g. `blue`.
- `unleash://checkout-experiment#payload` resolves to that variant's payload (often JSON, which you can `flatten:"json"`).

## Watch

The Unleash client refreshes its toggle state on its own interval; mamori polls (`WithPollInterval` + jitter) on top of that.

## Error classification

Unleash's client surface returns bool/string values, not errors, so this provider has no error vocabulary beyond not-found. It is conformance-exempt from the `ErrorClassification` case via `providertest.Config.NoResolveErrors`.

## Configuration

```go
import unleashprov "github.com/xavidop/mamori/providers/unleash"

mamori.WithProvider(unleashprov.New(
	unleashprov.WithURL(os.Getenv("UNLEASH_URL")),
	unleashprov.WithToken(os.Getenv("UNLEASH_API_TOKEN")),
	unleashprov.WithAppName("my-service"),
))
```

Verified with an injected fake (un-seeded toggles resolve to not-found); live behavior is covered by `//go:build integration` tests.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting the Unleash server. It releases the backing client, but only one this provider built itself, which stops its refresh and metrics goroutines. A client injected with `WithClient` belongs to the caller and is left running; closing it would stop those same goroutines out from under whatever else the caller is using it for.
