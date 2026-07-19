# mamori S3 provider

Amazon S3 (and S3-compatible: MinIO, Cloudflare R2, Backblaze B2, Ceph) value provider for [mamori](https://github.com/xavidop/mamori), built on `aws-sdk-go-v2`.

```bash
go get github.com/xavidop/mamori/providers/s3
```

```go
import _ "github.com/xavidop/mamori/providers/s3" // registers s3://
```

## Scheme

| Scheme | Backend | Sensitive | Watch |
|---|---|---|---|
| `s3://<bucket>/<key>[#json-key]` | Amazon S3 / S3-compatible | opt-in (`WithSensitive`) | poll (ETag) |

The `<key>` is an object key and may itself contain slashes, because object keys are paths: everything after the first `/` (the bucket segment) is the object key.

```go
type Config struct {
    App      map[string]any `source:"s3://my-bucket/config/app.json"`           // whole object
    DBPass   secret.String  `source:"s3://my-bucket/config/app.json#database"`  // one key of a JSON object
    TLSChain secret.Bytes   `source:"s3://my-bucket/secrets/tls/chain.pem"`     // nested key with slashes
}
```

- `#json-key` selects a single field from a JSON object payload (via `mamori.SelectKey`), identically to every other mamori provider.
- `Value.Version` is the object **ETag** (surrounding quotes stripped) when present, else the **VersionId**, else `mamori.VersionHash(bytes)`. It is computed from the whole object *before* `#json-key` selection, so it tracks changes to the underlying object regardless of which field is read.
- Values are **not** marked `Sensitive` by default. S3 buckets often hold secret bundles (JSON credentials, PEM chains, dotenv files); pass `WithSensitive(true)` to redact every resolved value downstream.

## Authentication

Uses the standard AWS credential chain (env vars, shared config/profile, IAM role, SSO, EC2/ECS/EKS role). Configure explicitly with options:

```go
// Amazon S3, pinned region, secret bundle.
mamori.WithProvider(s3.New(
    s3.WithRegion("eu-west-1"),
    s3.WithSensitive(true),
))

// MinIO (path-style is enabled automatically when an endpoint is set).
mamori.WithProvider(s3.New(
    s3.WithRegion("us-east-1"),
    s3.WithEndpoint("http://localhost:9000"),
))

// Cloudflare R2 (use region "auto").
mamori.WithProvider(s3.New(
    s3.WithRegion("auto"),
    s3.WithEndpoint("https://<accountid>.r2.cloudflarestorage.com"),
))
```

| Option | Purpose |
|---|---|
| `WithRegion(string)` | Pin the AWS region (required for signing against custom endpoints; R2 uses `"auto"`). |
| `WithEndpoint(string)` | Target MinIO / R2 / any S3-compatible endpoint. Enables path-style addressing. |
| `WithSensitive(bool)` | Mark every resolved value as secret so it is redacted downstream. |

## Watch

S3 has no cheap native change push, so this provider **does not** implement `WatchableProvider` - mamori polls it (interval + jitter), comparing the cheap ETag `Value.Version` to detect changes without re-reading unchanged objects on the caller's side. Configure the cadence with `mamori.WithPollInterval`.

A future push mode could bridge **S3 Event Notifications** through **SQS** or **EventBridge**, turning object writes into watch updates; it is not implemented here.

## What is verified

- ✅ Unit tests against an injected fake S3 client (object resolve, `#json-key` selection, nested slashed keys, `WithSensitive`, ETag / VersionId / `VersionHash` version precedence, `NoSuchKey` and `NoSuchBucket` → `ErrNotFound`, malformed-ref handling), plus the [`providertest`](../../providertest) conformance kit against an in-memory fake (`SkipWatch`).
- ⚠️ Live S3 / S3-compatible behavior is exercised by `//go:build integration` tests that require real credentials and a bucket, and are **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
