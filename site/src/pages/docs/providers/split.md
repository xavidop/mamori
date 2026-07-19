---
layout: ../../../layouts/DocsLayout.astro
title: Split provider
---

# Split

Load a [Split](https://www.split.io) (Harness FME) feature flag's treatment as config.

| | |
| --- | --- |
| Scheme | `split://` |
| Module | `github.com/xavidop/mamori/providers/split` |
| Sensitive | no |
| Watch | poll |
| Auth | `SPLIT_API_KEY` (server-side SDK key) |

## Install

```bash
go get github.com/xavidop/mamori/providers/split
```

```go
import _ "github.com/xavidop/mamori/providers/split"
```

## Using the ref

A `split://` ref points at one feature flag (split). It resolves to the treatment string returned for the evaluation key.

```text
split://<feature-flag>[?key=<traffic-key>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<feature-flag>` | yes | The Split feature flag name. |
| `?key=` | no | The traffic key (the identity evaluated). Defaults to `mamori`. |

The value is the treatment, which is a string you define in Split (commonly `on` / `off`, but it can be any named treatment). Split's special `control` treatment - returned when the flag is missing or the client is not ready - resolves to not-found.

**Examples**

- `split://new-onboarding` resolves to e.g. `on` or `off` - pair with `validate:"oneof=on off"`.
- `split://pricing-tier?key=service-a` resolves to the treatment for the `service-a` traffic key.

## Watch

The Split SDK synchronizes definitions in the background; mamori polls (`WithPollInterval` + jitter). The client is started and waited-until-ready lazily on first use.

## Configuration

```go
import splitprov "github.com/xavidop/mamori/providers/split"

mamori.WithProvider(splitprov.New(
	splitprov.WithAPIKey(os.Getenv("SPLIT_API_KEY")),
	splitprov.WithKey("my-service"),
))
```

Verified with an injected fake (un-seeded flags return `control` -> not-found); live behavior is covered by `//go:build integration` tests.
