---
layout: ../../../layouts/DocsLayout.astro
title: Providers overview
---

# Providers

A provider resolves one scheme. It implements `mamori.Provider` (`Scheme()` + `Resolve`), optionally `WatchableProvider` (native push) and `BatchProvider` (one call for many refs). Register with the `database/sql` pattern - a blank import is enough:

```go
import _ "github.com/xavidop/mamori/providers/vault"
```

Every provider that ships in this repo passes the conformance kit (see **Write a provider**). Pick one from the sidebar for its scheme, ref grammar, auth, and examples.

| Scheme | Page | Sensitive | Watch |
| --- | --- | --- | --- |
| `env:` | env | no | poll |
| `dotenv://` | dotenv | no | fsnotify |
| `file://` | file | no | fsnotify |
| `exec:` | exec | yes | poll |
| `aws-sm://` `aws-ps://` | AWS | yes / secure | poll |
| `vault://` | Vault | yes | lease-aware poll |
| `gcp-sm://` | GCP | yes | poll |
| `azure-kv://` | Azure | yes | poll |
| `doppler://` | Doppler | yes | poll |
| `op://` | 1Password | yes | poll |
| `sops://` | SOPS | yes | fsnotify |
| `postgres://` | PostgreSQL | no | **native** (LISTEN/NOTIFY) |
| `mysql://` | MySQL | no | poll |
| `sqlite://` | SQLite | no | fsnotify |
| `mongodb://` | MongoDB | no | **native** (change streams) |
| `dynamodb://` | DynamoDB | no | poll |
| `cosmos://` | Cosmos DB | no | poll (ETag) |
| `redis://` | Redis | no | **native** (keyspace) |
| `consul://` | Consul | no | **native** |
| `etcd://` | etcd | no | **native** |
| `k8s-secret://` `k8s-cm://` | Kubernetes | yes / no | **native** |
| `firestore://` | Firestore | no | **native** (snapshots) |
| `firebase-rc://` | Remote Config | no | poll |
| `firebase-rtdb://` | Realtime DB | no | **native** (streaming) |
| `s3://` | Amazon S3 | no | poll (ETag) |
| `gcs://` | Google GCS | no | poll (generation) |
| `azblob://` | Azure Blob | no | poll (ETag) |
| `launchdarkly://` | LaunchDarkly | no | **native** (streaming) |
| `unleash://` | Unleash | no | poll |
| `flagsmith://` | Flagsmith | no | poll |
| `configcat://` | ConfigCat | no | poll |
| `split://` | Split | no | poll |
| `growthbook://` | GrowthBook | no | poll |
| `flipt://` | Flipt | no | poll |
| `goff://` | GO Feature Flag | no | poll |

## Choosing and configuring a provider

Most providers auto-register a zero-config instance that reads ambient credentials (env vars, the AWS/GCP/Azure default credential chains, in-cluster Kubernetes config). A blank import is then all you need. When you must configure a provider explicitly - a region, an address, an injected client - construct it and pass it with `WithProvider`:

```go
import awsprov "github.com/xavidop/mamori/providers/aws"

cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(awsprov.NewSecretsManager(awsprov.WithRegion("eu-west-1"))),
)
```

`WithProvider` takes precedence over the registry for that scheme, for that call only.

## Watch behavior

- **native** - the backend pushes changes (Kubernetes watch API, Consul blocking queries). mamori subscribes directly.
- **fsnotify** - a local file is watched for writes (built-in `file://`, `sops://`).
- **lease-aware poll** - polling, but `Value.NotAfter` from a Vault lease triggers a refresh before expiry.
- **poll** - mamori polls on `WithPollInterval` with jitter, using `Value.Version` to detect change.

Provider authors implement the smallest interface native to their backend and never fake a watch with an internal ticker - mamori supplies the poller.
