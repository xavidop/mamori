---
layout: ../../../layouts/DocsLayout.astro
title: Google GCS provider
---

# Google Cloud Storage

Fetch a config or secret blob from a GCS bucket.

| | |
| --- | --- |
| Scheme | `gcs://` |
| Module | `github.com/xavidop/mamori/providers/gcs` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | poll (generation) |
| Auth | Application Default Credentials |

## Install

```bash
go get github.com/xavidop/mamori/providers/gcs
```

```go
import _ "github.com/xavidop/mamori/providers/gcs"
```

## Using the ref

A `gcs://` ref points at one object in a bucket.

```text
gcs://<bucket>/<object>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<bucket>` | yes | The GCS bucket name. |
| `<object>` | yes | The object name. It may contain slashes, e.g. `config/prod/app.json`. |
| `#json-key` | no | Treat the object as a JSON object and return one field of it. |

**Examples**

- `gcs://my-bucket/config/app.json` fetches the whole object - decode it with `flatten:"json"`.
- `gcs://my-bucket/app/config.json#feature_x` returns just the `feature_x` field of that JSON object.
- `gcs://my-bucket/env/prod/settings.yaml` fetches a nested object (the object name carries slashes).

```go
type Config struct {
	AppConfig AppConfig `source:"gcs://my-bucket/config/app.json" flatten:"json"`
}
```

The object name may contain slashes, so `env/prod/settings.yaml` is a single name. `Value.Version` is the object generation number (or ETag), which changes on every overwrite, so change detection is cheap. Objects are not marked sensitive by default; pass `WithSensitive()` for buckets that hold secret material.

## Watch

mamori polls (`WithPollInterval` + jitter) using the generation. For push, GCS Pub/Sub object-change notifications can trigger an on-demand reload.

## Configuration

```go
import gcsprov "github.com/xavidop/mamori/providers/gcs"

mamori.WithProvider(gcsprov.New()) // uses Application Default Credentials
```

Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
