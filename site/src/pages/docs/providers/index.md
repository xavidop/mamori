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

The **Errors** column shows which providers classify a failure beyond `not_found`, mapping backend-specific errors onto `mamori.ErrorKind` values like `permission_denied`, `unauthenticated`, `unavailable`, `rate_limited`, and `invalid`. The classification sweep across the whole catalog is now complete, and every provider falls into exactly one of three honest states:

- **✅** - classifies real backend errors beyond `not_found`: thirty-eight rows across thirty-three modules. A row lists every scheme its module classifies, so `aws-sm://` and `aws-ps://` share one and `k8s-secret://` and `k8s-cm://` share one, while the core module's `env:`, `dotenv://`, `file://` and `exec:` each get a row of their own.
- **no (chain preserved)** - `firebase-rtdb`, `growthbook`, and `flagsmith` have no backend-specific error vocabulary to map, so a non-not-found failure still reports `unknown`, but `Resolve` wraps the underlying error with `%w` rather than flattening it, so `errors.Is`/`errors.As` still reach it. This is not classification; it is proof that the chain survives even where there is nothing more specific to name.
- **n/a (no error surface)** - `unleash`, `configcat`, `split`, and `viper` wrap a client surface with no per-key error at all: the flag SDKs return only `bool`/`string`, and Viper's own read API has no error return (`Get` returns `any`, `IsSet` returns `bool`). `Resolve` can only ever produce `not_found` or a client-construction error. Each is explicitly exempt from the conformance kit's `ErrorClassification` case via `providertest.Config.NoResolveErrors`, a deliberate, greppable opt-out rather than a silent gap.

Don't read either non-✅ state as broken: `not_found` is detected everywhere regardless of this column, and neither state claims to see permission or availability errors that provider genuinely cannot observe.

| Scheme | Page | Sensitive | Watch | Errors |
| --- | --- | --- | --- | --- |
| `env:` | env | no | poll | ✅ |
| `dotenv://` | dotenv | no | fsnotify | ✅ |
| `file://` | file | no | fsnotify | ✅ |
| `exec:` | exec | yes | poll | ✅ |
| `mamori://` | mamori (client) | passthrough | **native** (SSE) | ✅ |
| `aws-sm://` `aws-ps://` | AWS | yes / secure | poll | ✅ |
| `aws-appconfig://` | AWS AppConfig | no | poll | ✅ |
| `vault://` | Vault | yes | lease-aware poll | ✅ |
| `gcp-sm://` | GCP | yes | poll | ✅ |
| `azure-kv://` | Azure | yes | poll | ✅ |
| `azure-appconfig://` | Azure AppConfig | no | poll | ✅ |
| `doppler://` | Doppler | yes | poll | ✅ |
| `hcp-vs://` | HCP Vault Secrets | yes | poll | ✅ |
| `scaleway-sm://` | Scaleway Secret Manager | yes | poll | ✅ |
| `bitwarden-sm://` | Bitwarden Secrets Manager | yes | poll | ✅ |
| `op://` | 1Password | yes | poll | ✅ |
| `sops://` | SOPS | yes | fsnotify | ✅ |
| `supabase://` | Supabase Vault | yes | poll | ✅ |
| `postgres://` | PostgreSQL | no | **native** (LISTEN/NOTIFY) | ✅ |
| `mysql://` | MySQL | no | poll | ✅ |
| `sqlite://` | SQLite | no | fsnotify | ✅ |
| `mongodb://` | MongoDB | no | **native** (change streams) | ✅ |
| `dynamodb://` | DynamoDB | no | poll | ✅ |
| `cosmos://` | Cosmos DB | no | poll (ETag) | ✅ |
| `redis://` | Redis | no | **native** (keyspace) | ✅ |
| `consul://` | Consul | no | **native** | ✅ |
| `etcd://` | etcd | no | **native** | ✅ |
| `nacos://` | Nacos | no | **native** | ✅ |
| `vercel-gc://` | Vercel Global Config | no | poll (digest) | ✅ |
| `cloudflare-kv://` | Cloudflare Workers KV | no | poll | ✅ |
| `heroku://` | Heroku Config Vars | yes | poll | ✅ |
| `https://` | Generic HTTPS | per-endpoint | poll (conditional GET) | ✅ |
| `k8s-secret://` `k8s-cm://` | Kubernetes | yes / no | **native** | ✅ |
| `firestore://` | Firestore | no | **native** (snapshots) | ✅ |
| `firebase-rc://` | Remote Config | no | poll | ✅ |
| `firebase-rtdb://` | Realtime DB | no | **native** (streaming) | no (chain preserved) |
| `s3://` | Amazon S3 | no | poll (ETag) | ✅ |
| `gcs://` | Google GCS | no | poll (generation) | ✅ |
| `azblob://` | Azure Blob | no | poll (ETag) | ✅ |
| `launchdarkly://` | LaunchDarkly | no | **native** (streaming) | ✅ |
| `unleash://` | Unleash | no | poll | n/a (no error surface) |
| `flagsmith://` | Flagsmith | no | poll | no (chain preserved) |
| `configcat://` | ConfigCat | no | poll | n/a (no error surface) |
| `split://` | Split | no | poll | n/a (no error surface) |
| `growthbook://` | GrowthBook | no | poll | no (chain preserved) |
| `flipt://` | Flipt | no | poll | ✅ |
| `goff://` | GO Feature Flag | no | poll | ✅ |
| `openfeature://` | OpenFeature | no | poll | ✅ |
| `posthog://` | PostHog | no | poll | ✅ |
| `viper://` | Viper | no | poll | n/a (no error surface) |

## Choosing and configuring a provider

Most providers auto-register a zero-config instance that reads ambient credentials (env vars, the AWS/GCP/Azure default credential chains, in-cluster Kubernetes config). A blank import is then all you need. When you must configure a provider explicitly - a region, an address, an injected client - construct it and pass it with `WithProvider`:

```go
import awsprov "github.com/xavidop/mamori/providers/aws"

cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(awsprov.NewSecretsManager(awsprov.WithRegion("eu-west-1"))),
)
```

`WithProvider` takes precedence over the registry for that scheme, for that call only.

The [mamori (client)](/docs/providers/mamori/) provider is the one shipped provider with no zero-config default at all: a `mamori://` binding name only means something relative to one specific config server, so it is always constructed explicitly with `mamoriprov.New(mamoriprov.Config{Endpoint: ...})` and passed via `WithProvider`, never registered from a blank import. It is also structurally different from every other row in the table above: it is not itself a secret manager, database, or flag service, it is a client for the [config server](/docs/server/), a fan-out process that fronts one of those backends and re-serves it to many callers. `Sensitive` and error classification are marked "passthrough" for it because both are whatever its upstream backend reports, carried through the hop unchanged, rather than a property fixed by this provider itself.

## Watch behavior

- **native** - the backend pushes changes (Kubernetes watch API, Consul blocking queries, a Nacos long-poll listener, a mamori config server's `/v1/watch` Server-Sent Events stream). mamori subscribes directly.
- **fsnotify** - a local file is watched for writes (built-in `file://`, `sops://`).
- **lease-aware poll** - polling, but `Value.NotAfter` from a Vault lease triggers a refresh before expiry.
- **poll** - mamori polls on `WithPollInterval` with jitter, using `Value.Version` to detect change.

Provider authors implement the smallest interface native to their backend and never fake a watch with an internal ticker - mamori supplies the poller.

Because mamori supplies the poller, it also supplies the retry cadence for the three polled rows: [`WithBackoff`](/docs/usage/watching/#retry-backoff) spaces out retries for a ref that keeps failing to resolve. It does not reach a **native** provider, which owns its stream and its own reconnect behavior; see each provider's page for that.
