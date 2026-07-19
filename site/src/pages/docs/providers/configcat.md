---
layout: ../../../layouts/DocsLayout.astro
title: ConfigCat provider
---

# ConfigCat

Load a [ConfigCat](https://configcat.com) setting's value as config.

| | |
| --- | --- |
| Scheme | `configcat://` |
| Module | `github.com/xavidop/mamori/providers/configcat` |
| Sensitive | no |
| Watch | poll |
| Auth | `CONFIGCAT_SDK_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/configcat
```

```go
import _ "github.com/xavidop/mamori/providers/configcat"
```

## Using the ref

A `configcat://` ref points at one setting key. It resolves to that setting's evaluated value.

```text
configcat://<setting-key>
```

| Part | Required | What it means |
| --- | --- | --- |
| `<setting-key>` | yes | The ConfigCat setting key. |

The value maps to bytes by type: a boolean setting resolves to `true` / `false`, a string to its string, a number to its decimal form. A key not present in the config resolves to not-found (mamori does not silently return the SDK default for a missing key).

**Examples**

- `configcat://isAwesomeFeatureEnabled` resolves to `true` / `false` - pair it with a `bool` field.
- `configcat://maxUploadSizeMB` resolves to e.g. `50` - pair it with an `int` field.

## Watch

The ConfigCat SDK auto-polls its config; mamori polls (`WithPollInterval` + jitter) on top.

## Configuration

```go
import configcatprov "github.com/xavidop/mamori/providers/configcat"

mamori.WithProvider(configcatprov.New(configcatprov.WithSDKKey(os.Getenv("CONFIGCAT_SDK_KEY"))))
```

Verified with an injected fake (un-seeded keys resolve to not-found); live behavior is covered by `//go:build integration` tests.
