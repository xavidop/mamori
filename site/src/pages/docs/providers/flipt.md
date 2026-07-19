---
layout: ../../../layouts/DocsLayout.astro
title: Flipt provider
---

# Flipt

Load a flag from [Flipt](https://www.flipt.io), the Go-native open-source feature-flag server.

| | |
| --- | --- |
| Scheme | `flipt://` |
| Module | `github.com/xavidop/mamori/providers/flipt` |
| Sensitive | no |
| Watch | poll |
| Auth | `FLIPT_URL` (+ optional token) |

## Install

```bash
go get github.com/xavidop/mamori/providers/flipt
```

```go
import _ "github.com/xavidop/mamori/providers/flipt"
```

## Using the ref

A `flipt://` ref points at one flag within a namespace, evaluated for an entity.

```text
flipt://<namespace>/<flag-key>[?entity=<id>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<namespace>` | yes | The Flipt namespace (use `default` if you have not created others). |
| `<flag-key>` | yes | The flag key. |
| `?entity=` | no | The evaluation entity id. Defaults to `mamori`. |

A boolean flag resolves to `true` / `false`; a variant flag resolves to the matched variant key. A flag that does not exist resolves to not-found.

**Examples**

- `flipt://default/new-billing` on a boolean flag resolves to `true` / `false`.
- `flipt://default/experiment-a?entity=service-1` on a variant flag resolves to the variant matched for `service-1`.

## Watch

Flipt is evaluated over its API; mamori polls (`WithPollInterval` + jitter).

## Configuration

```go
import fliptprov "github.com/xavidop/mamori/providers/flipt"

mamori.WithProvider(fliptprov.New(
	fliptprov.WithURL(os.Getenv("FLIPT_URL")),
	fliptprov.WithToken(os.Getenv("FLIPT_TOKEN")),
))
```

Verified with an injected fake (un-seeded flags resolve to not-found); live behavior is covered by `//go:build integration` tests.
