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

- **✅** - classifies real backend errors beyond `not_found`: forty-four rows across thirty-nine modules. A row lists every scheme its module classifies, so `aws-sm://` and `aws-ps://` share one and `k8s-secret://` and `k8s-cm://` share one, while the core module's `env:`, `dotenv://`, `file://` and `exec:` each get a row of their own.
- **no (chain preserved)** - `firebase-rtdb`, `growthbook`, and `flagsmith` have no backend-specific error vocabulary to map, so a non-not-found failure still reports `unknown`, but `Resolve` wraps the underlying error with `%w` rather than flattening it, so `errors.Is`/`errors.As` still reach it. This is not classification; it is proof that the chain survives even where there is nothing more specific to name.
- **n/a (no error surface)** - `unleash`, `configcat`, `split`, and `viper` wrap a client surface with no per-key error at all: the flag SDKs return only `bool`/`string`, and Viper's own read API has no error return (`Get` returns `any`, `IsSet` returns `bool`). `Resolve` can only ever produce `not_found` or a client-construction error. Each is explicitly exempt from the conformance kit's `ErrorClassification` case via `providertest.Config.NoResolveErrors`, a deliberate, greppable opt-out rather than a silent gap.

Don't read either non-✅ state as broken: `not_found` is detected everywhere regardless of this column, and neither state claims to see permission or availability errors that provider genuinely cannot observe.

The **Close** column says what each provider's `Close()` actually releases. mamori never closes a provider for you: a provider instance belongs to whoever constructed it, and `Watcher.Close()` releases only what mamori itself created (see [Who closes a provider](/docs/writing-a-provider/#who-closes-a-provider)). Thirty-one modules ship a `Close`; the rows reading **none needed** hold no releasable handle and have no `Close` method at all, so there is nothing to forget. Two rows read **terminal only, releases nothing**: `sqlite` opens and closes a fresh `*sql.DB` inside every `Resolve`, and `firebase-rtdb` holds no client of its own, but both keep a `Close` so that a caller sweeping `Close` across their siblings is not surprised to find them alone still serving. Where a cell says **injected**, the provider builds no default client it holds a reference to (or its default leaves `Transport` nil, which resolves to the shared `http.DefaultTransport` and is deliberately left alone), so `Close` releases idle connections only for a client you supplied; it never closes or invalidates that client. Whatever the cell says, `Close` is terminal: afterwards `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally. It does **not** stop a `Watch` that is already running, which is its own subject: see [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch). Each provider's own page has the full ownership rules.

| Scheme | Page | Sensitive | Watch | Errors | Close |
| --- | --- | --- | --- | --- | --- |
| `env:` | env | no | poll | ✅ | none needed |
| `dotenv://` | dotenv | no | fsnotify | ✅ | none needed |
| `file://` | file | no | fsnotify | ✅ | none needed |
| `exec:` | exec | yes | poll | ✅ | none needed |
| `mamori://` | mamori (client) | passthrough | **native** (SSE) | ✅ | each endpoint's idle HTTP conns |
| `aws-sm://` `aws-ps://` | AWS | yes / secure | poll | ✅ | none needed |
| `aws-appconfig://` | AWS AppConfig | no | poll | ✅ | none needed |
| `vault://` | Vault | yes | lease-aware poll | ✅ | none needed |
| `gcp-sm://` | GCP | yes | poll | ✅ | closes a self-built client |
| `azure-kv://` | Azure | yes | poll | ✅ | none needed |
| `azure-appconfig://` | Azure AppConfig | no | poll | ✅ | none needed |
| `doppler://` | Doppler | yes | poll | ✅ | injected HTTP client's idle conns |
| `infisical://` | Infisical | yes | poll | ✅ | injected HTTP client's idle conns |
| `hcp-vs://` | HCP Vault Secrets | yes | poll | ✅ | injected HTTP client's idle conns |
| `scaleway-sm://` | Scaleway Secret Manager | yes | poll | ✅ | injected HTTP client's idle conns |
| `bitwarden-sm://` | Bitwarden Secrets Manager | yes | poll | ✅ | injected HTTP client's idle conns |
| `op://` | 1Password | yes | poll | ✅ | injected HTTP client's idle conns |
| `sops://` | SOPS | yes | fsnotify | ✅ | none needed |
| `supabase://` | Supabase Vault | yes | poll | ✅ | injected HTTP client's idle conns |
| `postgres://` | PostgreSQL | no | **native** (LISTEN/NOTIFY) | ✅ | closes a self-opened pool |
| `mysql://` | MySQL | no | poll | ✅ | closes a self-opened `*sql.DB` |
| `sqlite://` | SQLite | no | fsnotify | ✅ | terminal only, releases nothing |
| `mongodb://` | MongoDB | no | **native** (change streams) | ✅ | disconnects a self-dialed client |
| `dynamodb://` | DynamoDB | no | poll | ✅ | none needed |
| `cosmos://` | Cosmos DB | no | poll (ETag) | ✅ | none needed |
| `redis://` | Redis | no | **native** (keyspace) | ✅ | closes a self-built client |
| `consul://` | Consul | no | **native** | ✅ | none needed |
| `etcd://` | etcd | no | **native** | ✅ | closes a self-dialed client |
| `nacos://` | Nacos | no | **native** | ✅ | injected HTTP client's idle conns |
| `vercel-gc://` | Vercel Global Config | no | poll (digest) | ✅ | injected HTTP client's idle conns |
| `cloudflare-kv://` | Cloudflare Workers KV | no | poll | ✅ | injected HTTP client's idle conns |
| `heroku://` | Heroku Config Vars | yes | poll | ✅ | injected HTTP client's idle conns |
| `https://` | Generic HTTPS | per-endpoint | poll (conditional GET) | ✅ | injected endpoint clients' idle conns |
| `k8s-secret://` `k8s-cm://` | Kubernetes | yes / no | **native** | ✅ | self-built clientset's idle conns |
| `firestore://` | Firestore | no | **native** (snapshots) | ✅ | closes a self-built client |
| `firebase-rc://` | Remote Config | no | poll | ✅ | injected HTTP client's idle conns |
| `firebase-rtdb://` | Realtime DB | no | **native** (streaming) | no (chain preserved) | terminal only, releases nothing |
| `s3://` | Amazon S3 | no | poll (ETag) | ✅ | none needed |
| `gcs://` | Google GCS | no | poll (generation) | ✅ | closes a self-built reader client |
| `azblob://` | Azure Blob | no | poll (ETag) | ✅ | none needed |
| `launchdarkly://` | LaunchDarkly | no | **native** (streaming) | ✅ | closes a self-built client |
| `unleash://` | Unleash | no | poll | n/a (no error surface) | closes a self-built client |
| `flagsmith://` | Flagsmith | no | poll | no (chain preserved) | none needed |
| `configcat://` | ConfigCat | no | poll | n/a (no error surface) | closes the SDK client (stops polling) |
| `split://` | Split | no | poll | n/a (no error surface) | destroys a self-built client |
| `growthbook://` | GrowthBook | no | poll | no (chain preserved) | closes the SDK client |
| `flipt://` | Flipt | no | poll | ✅ | none needed |
| `goff://` | GO Feature Flag | no | poll | ✅ | none needed |
| `openfeature://` | OpenFeature | no | poll | ✅ | none needed |
| `posthog://` | PostHog | no | poll | ✅ | injected HTTP client's idle conns |
| `viper://` | Viper | no | poll | n/a (no error surface) | none needed |

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
