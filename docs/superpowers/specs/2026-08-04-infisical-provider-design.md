# Infisical provider

**Goal:** `infisical://` resolves secrets from [Infisical](https://infisical.com), the open-source secret manager, as a thin adapter over `providers/httpcore`.

**Status:** design approved, pending spec review.

**Programme:** PR2 of the sixteen in `2026-08-03-provider-and-core-expansion-design.md`. The first consumer of `httpcore` other than `providers/https`, so it is also the first real test of whether that API carries a second provider.

## Vendor contract, pinned from the live reference

Verified against Infisical's API reference on 2026-08-04. Note the read path is **v4**,
not the v3 most third-party write-ups still describe.

```
POST /api/v1/auth/universal-auth/login
  Content-Type: application/json
  {"clientId": "...", "clientSecret": "..."}
  -> 200 {"accessToken": "...", "expiresIn": 3600,
          "accessTokenMaxTTL": 2592000, "tokenType": "Bearer"}

GET /api/v4/secrets/{secretName}?projectId=&environment=&secretPath=
  Authorization: Bearer <accessToken>
  -> 200 {"secret": {"secretKey": "...", "secretValue": "...", "version": 3, ...}}

Failures: 400, 401, 403, 404, 422, 500
```

`projectId` is required. `environment` and `secretPath` are optional, `secretPath`
defaulting to `/`.

Sources: <https://infisical.com/docs/api-reference/endpoints/secrets/read>,
<https://infisical.com/docs/api-reference/endpoints/universal-auth/login>.

## What this provider proves about httpcore

Two things came out of pinning the contract, and both were fixed in PR1 before this
provider existed:

**422 was misclassified.** Infisical returns `422 Unprocessable Entity`.
`httpcore.ClassifyStatus` mapped anything unrecognised to `ErrUnavailable`, a transient
kind, so mamori would have backed off and retried a request that can never succeed.
Fixed in PR1: 422 now maps to `ErrInvalid`.

**`OAuth2ClientCredentials` does not fit.** It posts RFC 6749 form fields
(`grant_type`, `client_id`, `client_secret`) and reads `access_token`/`expires_in`.
Infisical posts **JSON** `{clientId, clientSecret}` to a non-standard path and reads
`accessToken`/`expiresIn`. So this provider writes its own token authenticator.

That is deliberate, not a gap. HCP Vault Secrets in PR3 uses genuine OAuth2
client-credentials and will reuse `httpcore`'s. If a shared shape emerges across PRs 3
through 8, it gets extracted then, following this project's rule that an abstraction
lands with its second consumer rather than ahead of it.

## Scheme

```
infisical://<secretName>[#<key>][?<opts>]
```

The whole ref path is the secret name, matching `providers/cloudflare-kv`'s precedent
that a key containing slashes is one key rather than a path. Project, environment and
secret path come from provider options, each overridable per ref:

| Option | Provider option | Ref option | Environment |
| --- | --- | --- | --- |
| Project id | `WithProjectID` | `?project=` | `INFISICAL_PROJECT_ID` |
| Environment | `WithEnvironment` | `?env=` | `INFISICAL_ENVIRONMENT` |
| Secret path | `WithSecretPath` | `?path=` | `INFISICAL_SECRET_PATH` |

A ref option wins over a provider option, which wins over the environment. This is
exactly `providers/cloudflare-kv`'s namespace precedence, so an operator who knows one
knows the other.

`#key` selects into the resolved value through `mamori.SelectKey`, so a secret holding
JSON supports both a top-level key and an RFC 6901 pointer, identically to every other
provider.

## Authentication

Machine identity Universal Auth: a client id and client secret exchange for a
short-lived access token, cached and refreshed before expiry.

`WithClientID` / `WithClientSecret`, or `INFISICAL_CLIENT_ID` /
`INFISICAL_CLIENT_SECRET`. An explicit option wins over its environment variable.

The token exchange is a `httpcore.Client` call, so it inherits body bounding, status
classification and URL redaction. The client secret is held in a closure rather than a
struct field, matching what `httpcore`'s own OAuth2 authenticator does, because `fmt`
reaches an unexported struct field by reflection and cannot be stopped by a `String`
method.

`WithBaseURL` overrides `https://app.infisical.com` for self-hosted installs. It
rejects a scheme outside `{https, http}` and requires `AllowInsecure` for `http`,
matching `providers/https`.

## Values

`Value.Bytes` is `secret.secretValue`. `Value.Version` is `secret.version` rendered as
a string, so mamori gets cheap change detection from the backend's own revision rather
than a content hash. `Value.Sensitive` is always true: this is a secret manager.

## Errors

`httpcore.ClassifyStatus` covers the mapping. `404` becomes `ErrNotFound` so a field's
`default:` and `optional` handling apply. `ErrorDetail` extracts Infisical's `message`
field so an operator sees why a request was refused, which is what `httpcore` added
that hook for.

## Watch

The read API exposes no streaming or blocking read, so this provider deliberately does
not implement `mamori.WatchableProvider` and mamori wraps it in the polling adapter.
Pinned by `TestProviderIsNotWatchable`.

## Documentation

Per the standing rule: `providers/infisical/README.md`, a docs-site page and sidebar
entry, both coverage tables, the root README install block, and
`skills/mamori/references/providers.md`.

## Risks

**The wire shape is documented, not exercised.** Nobody here has credentials, so the
JSON shapes come from the vendor reference rather than a live call. Mitigation: the
documentation URL is cited in the README's `Testing status` table, that table says
plainly which rows are unverified, and a `//go:build integration` test lets the first
person with credentials confirm the shape in one command.

**Infisical versions its read API.** This targets v4 while much of the ecosystem still
describes v3. `WithAPIVersion` is deliberately **not** offered: a second version means
a second response shape, and guessing at one nobody has tested is worse than requiring
a provider update.
