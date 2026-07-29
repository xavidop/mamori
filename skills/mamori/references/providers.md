# mamori providers reference

Each provider is a separate Go module (except the core built-ins). Add the
module, then blank-import it so it registers its scheme. Ref syntax is
`scheme://path#key?opts`; `#key` selects a field from a structured payload.

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
| `onepassword` | `op://` | `op://vault/item/field` |
| `sops` | `sops://` | `sops://secrets.enc.yaml#key` |
| `k8s` | `k8s-secret://` `k8s-cm://` | `k8s-secret://ns/name#key` |
| `consul` | `consul://` | `consul://app/config` |
| `etcd` | `etcd://` | `etcd://app/config` |
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
| `openfeature` | `openfeature://` | vendor-neutral flag standard; evaluates through whatever `openfeature.FeatureProvider` your app installs. `openfeature://new-checkout?type=bool`, `openfeature://limits#/upload/maxMB?type=object` |
| `viper` | `viper://` | reads whatever a `*viper.Viper` resolved for a key, inheriting its precedence (Set > flags > env > config file > k/v store > defaults); for incremental migration off an existing Viper setup. `viper://server.port`, `viper://db#/creds/port` |
| `mamori` | `mamori://` | client of a mamori config server (native watch) |

## Secret-bearing schemes

Store these in `secret.String` / `secret.Bytes`, never a plain `string`:

- Always secret: `aws-sm`, `gcp-sm`, `azure-kv`, `vault`, `op`, `sops`,
  `doppler`, `k8s-secret`.
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
