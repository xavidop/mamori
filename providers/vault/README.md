# mamori Vault provider

A [mamori](https://github.com/xavidop/mamori) provider for **HashiCorp Vault**'s
KV v2 secrets engine, with lease-aware refresh for dynamic secrets.

```
import _ "github.com/xavidop/mamori/providers/vault"
```

Blank-importing the package registers the `vault` scheme (from ambient
`VAULT_ADDR` / `VAULT_TOKEN`). For explicit configuration, register an instance:

```go
mamori.WithProvider(vault.New(
    vault.WithAddress("https://vault.example.com:8200"),
    vault.WithToken(tok),
    vault.WithNamespace("team-a"), // Vault Enterprise, optional
))
```

## Scheme

```
vault://<mount>/<path>[#key][?renew=true]
```

- `<mount>` - the KV v2 mount point (e.g. `secret`).
- `<path>` - the logical secret path within that mount.
- `#key` - optional: select a single field from the secret's data map.
- `?renew=true` - optional: renew the lease on read (dynamic secrets only).

### KV v2 and the `/data/` prefix

KV v2 physically stores secrets under `<mount>/data/<path>`, but you write the
**logical** path in the ref - the provider inserts `data/` for you (via
`client.KVv2(mount).Get`). A leading `data/` in the path is tolerated and
stripped, so both forms below resolve the same secret:

```
vault://secret/myapp/config        # logical form (preferred)
vault://secret/data/myapp/config   # physical form (also accepted)
```

### Ref examples

```go
type Config struct {
    // Whole secret data map, JSON-encoded: {"password":"...","username":"..."}
    DBCreds []byte `source:"vault://secret/myapp/db"`

    // A single field of the data map.
    Password string `source:"vault://secret/myapp/db#password"`
    APIKey   string `source:"vault://secret/myapp/config#api_key"`

    // A dynamic secret (database creds). The lease drives pre-expiry refresh;
    // renew=true extends the lease on each read. Note the mamori grammar order:
    // #key comes BEFORE ?opts.
    DynPassword string `source:"vault://database/creds/readonly#password?renew=true"`
}
```

## Behavior

- **Payload.** With no `#key`, `Value.Bytes` is the JSON encoding of the
  secret's data map. With `#key`, the field is selected via `mamori.SelectKey`
  (string fields returned unquoted; objects/arrays/numbers as their JSON), so
  `#key` behaves identically to every other mamori provider.
- **Version.** `Value.Version` is the KV v2 metadata version (e.g. `"3"`),
  which changes on every write. For non-versioned/logical reads it falls back to
  `mamori.VersionHash(data)`.
- **Sensitive.** Always `true` - Vault values are secrets and are redacted
  downstream. The payload is never logged.
- **Not found.** A missing secret (KV v2 `ErrSecretNotFound`, a 404 response, or
  a nil/empty data map) is mapped to `mamori.ErrNotFound`; a missing `#key` also
  yields `ErrNotFound`.

### Lease awareness and NotAfter

When the underlying Vault response carries a lease (`LeaseDuration > 0`, as with
dynamic secrets from the `database`, `aws`, `pki`, ... engines), the provider
sets `Value.NotAfter = now + LeaseDuration`. mamori schedules a refresh **before**
that instant rather than waiting for the next poll tick, so a value is renewed
ahead of lease expiry.

With `?renew=true`, if the lease has a renewable `LeaseID`, the provider calls
`Sys().Renew` on read and derives `NotAfter` from the renewed lease duration. A
renew failure is non-fatal: the freshly-read value is still returned with the
original lease-derived `NotAfter`.

Static KV v2 secrets have no lease, so `NotAfter` is zero and mamori refreshes
on its normal poll interval.

### Watch = lease-aware polling (no native Watch)

Vault KV has **no native change-notification** mechanism. Per the mamori SPI, a
provider must implement `Watch` only when the backend can push changes;
otherwise it must let mamori poll. This provider therefore **does not implement
`WatchableProvider`**. mamori wraps it in its polling adapter, and `NotAfter`
(above) triggers pre-expiry refresh for leased secrets. This is deliberate - the
provider never fakes a watch with an internal ticker.

## Authentication

The lazily-constructed client uses the standard Vault environment:

| Variable      | Purpose                                             |
| ------------- | --------------------------------------------------- |
| `VAULT_ADDR`  | Server address (default `https://127.0.0.1:8200`).  |
| `VAULT_TOKEN` | Auth token.                                         |

plus the usual `VAULT_CACERT`, `VAULT_NAMESPACE`, etc. honored by
`api.DefaultConfig()`. Options `WithAddress` / `WithToken` / `WithNamespace`
override the environment, and `WithClient(*api.Client)` injects a fully
preconfigured client (for AppRole, Kubernetes auth, custom TLS, and the like).

## Verified vs. needs a live backend

| Area                                                          | Status                                     |
| ------------------------------------------------------------- | ------------------------------------------ |
| Resolve (whole map / `#key`), not-found, version monotonicity | Verified - unit + conformance against an in-memory fake |
| Lease-derived `NotAfter`, `?renew=true` renewal path          | Verified - unit tests against the fake     |
| Context cancellation, concurrency, goroutine hygiene          | Verified - `providertest.Run`              |
| End-to-end against a real Vault KV v2 engine                  | **Needs a live backend** - see below       |

The default `go test ./...` runs entirely against an in-memory fake (no network,
no live Vault). Live tests are guarded behind the `integration` build tag:

```sh
vault server -dev -dev-root-token-id=root
export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root
go test -tags=integration ./...
```

They seed and read through a real KV v2 mount (`secret` in dev mode) and run the
full conformance kit live. They `t.Skip` when `VAULT_ADDR` / `VAULT_TOKEN` are
unset.

## Conformance

This provider passes the mamori conformance kit (`providertest.Run`): scheme,
resolve, typed not-found, context cancellation, concurrent resolve, and version
monotonicity. Watch tests are auto-skipped because the provider is not
watchable (see above).

```
go test ./...   # conformance + unit tests, no live Vault required
```
