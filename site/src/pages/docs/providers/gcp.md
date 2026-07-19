---
layout: ../../../layouts/DocsLayout.astro
title: GCP provider
---

# GCP

Google Cloud Secret Manager, built on `cloud.google.com/go/secretmanager`.

| | |
| --- | --- |
| Scheme | `gcp-sm://` |
| Module | `github.com/xavidop/mamori/providers/gcp` |
| Sensitive | yes |
| Watch | poll |
| Auth | Application Default Credentials |

## Install

```bash
go get github.com/xavidop/mamori/providers/gcp
```

```go
import _ "github.com/xavidop/mamori/providers/gcp"
```

## Using the ref

A `gcp-sm://` ref points at one secret in a GCP project's Secret Manager, at a specific (or the latest) version.

```text
gcp-sm://<project>/<secret>[#json-key][?version=<v>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<project>` | yes | The GCP project ID (or number). |
| `<secret>` | yes | The secret ID within that project. |
| `#json-key` | no | Select one field from a JSON secret payload (via `mamori.SelectKey`). |
| `?version=<v>` | no | Pin a specific secret version. Defaults to `latest`. |

**Examples**

- `gcp-sm://my-project/db-password` reads the latest version of `db-password`.
- `gcp-sm://my-project/api-key?version=3` pins version `3`, so the value never changes under you.
- `gcp-sm://my-project/creds#password` selects the `password` field of a JSON secret.

```go
type Config struct {
	DBPassword secret.String `source:"gcp-sm://my-project/db-password"`            // latest version
	APIKey     secret.String `source:"gcp-sm://my-project/api-key?version=3"`      // pinned version
	Nested     secret.String `source:"gcp-sm://my-project/creds#password"`         // key of a JSON secret
}
```

Values are always `Sensitive`, and `Value.Version` is the resolved secret version name (e.g. `.../versions/3`), so change detection is cheap.

## Explicit configuration

Authentication uses Application Default Credentials (a service-account key file via `GOOGLE_APPLICATION_CREDENTIALS`, workload identity, or the metadata server). For tests or custom transports, inject a client:

```go
import gcpprov "github.com/xavidop/mamori/providers/gcp"

mamori.WithProvider(gcpprov.New(gcpprov.WithClient(myClient)))
```

## Watch

Secret Manager has no native change notification, so mamori polls (`WithPollInterval` + jitter). Pub/Sub rotation notifications can drive an on-demand `Load` in your app if you need push.

Verified by unit tests and the conformance kit against an in-memory fake; live GCP behavior is covered by `//go:build integration` tests.
