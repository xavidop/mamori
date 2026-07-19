---
layout: ../../../layouts/DocsLayout.astro
title: GO Feature Flag provider
---

# GO Feature Flag

Load a flag from [GO Feature Flag](https://gofeatureflag.org), the Go-native open-source flag engine.

| | |
| --- | --- |
| Scheme | `goff://` |
| Module | `github.com/xavidop/mamori/providers/goff` |
| Sensitive | no |
| Watch | poll |
| Auth | flag config retriever (file / URL / bucket) |

## Install

```bash
go get github.com/xavidop/mamori/providers/goff
```

```go
import _ "github.com/xavidop/mamori/providers/goff"
```

## Using the ref

A `goff://` ref points at one flag key. It resolves to that flag's evaluated variation for a fixed evaluation context (your service).

```text
goff://<flag-key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<flag-key>` | yes | The GO Feature Flag flag key. |
| `#json-key` | no | Select one field from a JSON variation (via SelectKey). |

The variation maps to bytes by type (boolean, string, number, or JSON). A flag that does not exist resolves to not-found.

**Examples**

- `goff://new-feature` resolves to `true` / `false`.
- `goff://rollout-config#percentage` resolves to the `percentage` field of a JSON variation.

The evaluation context defaults to a stable targeting key (`mamori`); override it with `WithTargetingKey`.

## Watch

GO Feature Flag reloads its flag definitions from the configured retriever on an interval; mamori polls (`WithPollInterval` + jitter).

## Configuration

GO Feature Flag reads its flag definitions from a retriever - a local file, an HTTP URL, or an object-storage bucket:

```go
import goffprov "github.com/xavidop/mamori/providers/goff"

mamori.WithProvider(goffprov.New(goffprov.WithConfigFile("/etc/goff/flags.yaml")))
// or from a URL:
mamori.WithProvider(goffprov.New(goffprov.WithConfigURL("https://cdn.example.com/flags.yaml")))
```

Verified with an injected fake (un-seeded flags resolve to not-found); live behavior is covered by `//go:build integration` tests.
