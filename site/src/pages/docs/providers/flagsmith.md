---
layout: ../../../layouts/DocsLayout.astro
title: Flagsmith provider
---

# Flagsmith

Load a [Flagsmith](https://www.flagsmith.com) flag's value or state as config.

| | |
| --- | --- |
| Scheme | `flagsmith://` |
| Module | `github.com/xavidop/mamori/providers/flagsmith` |
| Sensitive | no |
| Watch | poll |
| Auth | `FLAGSMITH_ENVIRONMENT_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/flagsmith
```

```go
import _ "github.com/xavidop/mamori/providers/flagsmith"
```

## Using the ref

A `flagsmith://` ref points at one flag. Without a fragment it resolves to the flag's value; `#enabled` resolves to its on/off state.

```text
flagsmith://<feature-name>[#enabled]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<feature-name>` | yes | The Flagsmith feature name. |
| `#enabled` | no | Resolve to the feature's enabled state (`true` / `false`) instead of its value. |

A feature that does not exist resolves to not-found.

**Examples**

- `flagsmith://banner_text` resolves to the feature's value (a string, number, or JSON).
- `flagsmith://new_dashboard#enabled` resolves to `true` / `false` - pair it with a `bool` field.

## Watch

mamori polls (`WithPollInterval` + jitter); the Flagsmith client also refreshes internally.

## Configuration

```go
import flagsmithprov "github.com/xavidop/mamori/providers/flagsmith"

mamori.WithProvider(flagsmithprov.New(
	flagsmithprov.WithEnvironmentKey(os.Getenv("FLAGSMITH_ENVIRONMENT_KEY")),
))
// self-hosted: add flagsmithprov.WithBaseURL("https://flagsmith.internal/api/v1/")
```

Verified with an injected fake (un-seeded features resolve to not-found); live behavior is covered by `//go:build integration` tests.
