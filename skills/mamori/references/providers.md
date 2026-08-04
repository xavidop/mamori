# mamori providers reference

Each provider is a separate Go module (except the core built-ins). Add the
module, then blank-import it so it registers its scheme - except `https`,
whose `New` takes operator-supplied endpoints and returns an error, so it
needs an ordinary import and an explicit `New(...)` call instead (see the
`https` row below). Ref syntax is `scheme://path#key?opts`; `#key` selects a
field from a structured payload.

For authentication details and every option, see https://mamorigo.dev/docs/providers .

## Core built-ins (no extra module)

| Scheme | Ref example | Notes |
| --- | --- | --- |
| `env:` | `env:LOG_LEVEL` | Environment variable. |
| `file://` | `file:///etc/tls/tls.crt` | File contents; watched with fsnotify. |
| `dotenv://` | `dotenv://.env#KEY` | A key from a dotenv file. |
| `exec:` | `exec:./get-secret.sh` | Command stdout. Off by default; enable with `mamori.WithExecProvider()`. |

## Separate modules

Module path is `github.com/xavidop/mamori/providers/<name>`.

| Module | Scheme(s) | Ref example |
| --- | --- | --- |
| `aws` | `aws-sm://` `aws-ps://` `aws-appconfig://` | `aws-sm://prod/db#password`, `aws-ps://svc/port`, `aws-appconfig://myapp/prod/flags#/db/port` |
| `vault` | `vault://` | `vault://kv/data/api#token` |
| `gcp` | `gcp-sm://` | `gcp-sm://my-project/api-key` |
| `azure` | `azure-kv://` `azure-appconfig://` | `azure-kv://vaultname/secret-name`, `azure-appconfig://mystore/db/port?label=prod` |
| `doppler` | `doppler://` | `doppler://project/config#SECRET` |
| `infisical` | `infisical://` | `infisical://DB_PASSWORD`, `infisical://DB_PASSWORD?env=staging&path=/backend`, `infisical://DB_CREDS#/creds/password`. The whole ref path is the secret NAME; project/environment/folder come from options or `?project=`/`?env=`/`?path=`, never from the path. Machine identity Universal Auth via `INFISICAL_CLIENT_ID`/`INFISICAL_CLIENT_SECRET`. |
| `scaleway-sm` | `scaleway-sm://` | `scaleway-sm://prod/db-password`, `scaleway-sm://db-password?revision=7` |
| `bitwarden` | `bitwarden-sm://` | Bitwarden **Secrets Manager** (the `bws` machine-account product), not the consumer password manager. A secret is addressed by its **UUID**, never its name, because the list endpoint returns names as ciphertext and omits values: `bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d`, `bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#password`. Reads `BWS_ACCESS_TOKEN`. Decrypts client side with the Go standard library alone. |
| `onepassword` | `op://` | `op://vault/item/field` |
| `sops` | `sops://` | `sops://secrets.enc.yaml#key` |
| `k8s` | `k8s-secret://` `k8s-cm://` | `k8s-secret://ns/name#key` |
| `consul` | `consul://` | `consul://app/config` |
| `etcd` | `etcd://` | `etcd://app/config` |
| `vercel-gc` | `vercel-gc://` | `vercel-gc://my-flag`, `vercel-gc://ecfg_abc/my-flag` |
| `cloudflare-kv` | `cloudflare-kv://` | `cloudflare-kv://log-level`, `cloudflare-kv://log-level?namespace=abcd1234` |
| `https` | `https://` | generic REST config/secrets. The authority is a **registered `Endpoint.Name`, not a hostname** - `https://billing/cfg#/db/pass` resolves against whatever `BaseURL` the endpoint named `billing` was registered with via `httpsprov.New`, never against a host literally named `billing`. **This is the one module you do NOT blank-import**: it has no `init`, because an `Endpoint` is entirely operator-supplied and `New` returns an error. Import it normally, call `httpsprov.New(...)`, and pass the result to `mamori.WithProvider`. |
| `postgres` | `postgres://` | connection-string backed |
| `mysql` | `mysql://` | connection-string backed |
| `sqlite` | `sqlite://` | file-backed |
| `mongodb` | `mongodb://` | |
| `dynamodb` | `dynamodb://` | |
| `cosmos` | `cosmos://` | Azure Cosmos DB |
| `redis` | `redis://` | |
| `firestore` | `firestore://` | |
| `firebase-rc` | firebase Remote Config | |
| `firebase-rtdb` | firebase Realtime DB | |
| `s3` | `s3://` | Amazon S3 object |
| `gcs` | `gcs://` | Google Cloud Storage object |
| `azblob` | `azblob://` | Azure Blob Storage object |
| `launchdarkly` `unleash` `flagsmith` `configcat` `split` `growthbook` `flipt` `goff` | feature-flag schemes | one module per flag backend |
| `posthog` | `posthog://` | PostHog flag evaluated for one distinct id (set provider-wide with `WithDistinctID`, not per ref). No fragment gives `true`/`false` for a boolean flag and the variant key for a multivariate one; `#enabled`, `#variant`, `#payload` name a facet. `posthog://new-checkout`, `posthog://pricing-test#variant` |
| `openfeature` | `openfeature://` | vendor-neutral flag standard; evaluates through whatever `openfeature.FeatureProvider` your app installs. `openfeature://new-checkout?type=bool`, `openfeature://limits#/upload/maxMB?type=object` |
| `viper` | `viper://` | reads whatever a `*viper.Viper` resolved for a key, inheriting its precedence (Set > flags > env > config file > k/v store > defaults); for incremental migration off an existing Viper setup. `viper://server.port`, `viper://db#/creds/port` |
| `mamori` | `mamori://` | client of a mamori config server (native watch) |

## Secret-bearing schemes

Store these in `secret.String` / `secret.Bytes`, never a plain `string`:

- Always secret: `aws-sm`, `gcp-sm`, `azure-kv`, `vault`, `op`, `sops`,
  `doppler`, `scaleway-sm`, `k8s-secret`, `bitwarden-sm`.
- Sometimes secret, and flagged anyway: `aws-ps` (SecureString params), `exec`
  (mamori marks all command output secret), `mamori` (relays whatever the
  server marks).

`mamori vet` flags a secret scheme stored in a plain type. `k8s-cm` (ConfigMap),
`aws-appconfig`, and `azure-appconfig` (config services, not secret stores) -
and config schemes (`env`, `file`, `consul`, ...) - are not secret-bearing. For
a custom provider, add its scheme: `mamori vet --secret-schemes=mysecrets ./...`.

## Composition

- Precedence chain: `source:"env:PORT,aws-ps://svc/port"` (first that resolves
  wins; `onfail` handles a real error on the winner).
- Middleware wraps any provider: `middleware.Cache`, `Audit`, `RateLimit`,
  `Failover`, `Prefix` (see https://mamorigo.dev/docs/middleware ).

## Writing a new provider over a REST API

`providers/httpcore` is not a provider and registers no scheme: it is the
stdlib-only library the REST-backed providers are built on. Use it rather than
hand-rolling `net/http`, and you inherit request building, `Bearer`/header/
basic/query/OAuth2 authenticators, `ClassifyStatus` (plus its exact inverse
`StatusForKind` for the conformance `Fail` hook), conditional GET via
`Revalidator`, a bounded and always-drained response body, and a request path
that cannot traverse out of the `BaseURL`. See
https://mamorigo.dev/docs/writing-a-provider/httpcore .
