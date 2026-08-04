# mamori - HCP Vault Secrets provider

Resolve secrets from [HCP Vault Secrets](https://developer.hashicorp.com/hcp/docs/vault-secrets),
the hosted secret-management service on HashiCorp Cloud Platform.

Built on [`providers/httpcore`](../httpcore) and the standard library only.
HashiCorp publishes `github.com/hashicorp/hcp-sdk-go`, and this module
deliberately does not use it: the read path is a documented HTTPS API, so
staying on `net/http` keeps the SDK's dependency tree out of every consumer's
build.

## This is not `providers/vault`

HCP Vault Secrets and self-hosted HashiCorp Vault are **different products with
different APIs**, and this module covers only the first.

| | `providers/vault` | `providers/hcp-vault-secrets` |
| --- | --- | --- |
| Product | Vault you run (OSS or Enterprise) | HCP Vault Secrets (SaaS) |
| Scheme | `vault://` | `hcp-vs://` |
| Host | your cluster | `api.cloud.hashicorp.com` |
| Auth | Vault token, `X-Vault-Token` | HCP service principal, OAuth2 bearer |
| Addressing | mount + KV v2 path | organization + project + app + name |

Neither can resolve the other's refs. If you run your own Vault, you want
`providers/vault`.

## Scheme

```
hcp-vs://<secretName>[#<key>][?<opts>]
```

The **entire ref path is the secret name**, including any slashes it contains,
matching `providers/cloudflare-kv` and `providers/infisical`. The organization,
project and application that scope that name are therefore never taken from the
path: they come from provider options, each overridable per ref.

| Option | Overrides | Falls back to |
| --- | --- | --- |
| `?org=` | `WithOrganizationID` | `HCP_ORGANIZATION_ID` |
| `?project=` | `WithProjectID` | `HCP_PROJECT_ID` |
| `?app=` | `WithAppName` | `HCP_APP_NAME` |

Precedence is **ref option, then provider option, then environment variable**.

### Ref examples

| Ref | Resolves |
| --- | --- |
| `hcp-vs://DB_PASSWORD` | `DB_PASSWORD` in the configured app |
| `hcp-vs://DB_PASSWORD?app=web` | the same name in the `web` app |
| `hcp-vs://DB_CREDS#/creds/password` | a JSON Pointer into a JSON secret |
| `hcp-vs://DB_CREDS#password` | a literal top-level key |
| `hcp-vs://a%2Fb` | the single secret named `a/b` |

A name containing a slash is written percent-encoded. `mamori.ParseRef` does no
decoding, so `ref.Path` is already the escaped form `httpcore.Request.Path`
takes; this provider passes it straight through. Writing it unescaped would ask
for a different, nested resource, and re-escaping it here would both address the
wrong secret and silently disable httpcore's traversal check.

## Pinned API contract

Nobody on this project has HCP credentials. Every row below is taken from
HashiCorp's live documentation and is **documented, not live-verified**. The
`//go:build integration` tests exist to close exactly this gap in one command.

| What | Value | Source |
| --- | --- | --- |
| Token endpoint | `POST https://auth.idp.hashicorp.com/oauth2/token` | [HCP API overview](https://developer.hashicorp.com/hcp/docs/hcp/api) |
| Token body | `application/x-www-form-urlencoded`: `client_id`, `client_secret`, `grant_type=client_credentials`, `audience=https://api.hashicorp.cloud` | same |
| Token response | `{"access_token":"...","expires_in":3600,"token_type":"Bearer"}` | same |
| Request auth | `Authorization: Bearer <access_token>` | same |
| Read endpoint | `GET https://api.cloud.hashicorp.com/secrets/2023-11-28/organizations/{organization_id}/projects/{project_id}/apps/{app_name}/secrets/{secret_name}:open` | [OpenAppSecret](https://developer.hashicorp.com/hcp/api-docs/vault-secrets/openappsecret) |
| Path params | `organization_id`, `project_id`, `app_name`, `secret_name`, all required | same |
| Query params | none | same |
| Success shape | `{"secret":{"name","type","latest_version","static_version":{"version","value"}}}` | same |
| Error shape | `googlerpcStatus`: `{"code","message","details"}` | same |

### Two documented contradictions, and how they were resolved

**The token endpoint.** HashiCorp's support article
["HCP Vault Secrets: get, create and delete secrets via API"](https://support.hashicorp.com/hc/en-us/articles/18221966166291-HCP-Vault-Secrets-get-create-and-delete-secrets-via-API)
shows `POST https://auth.hashicorp.com/oauth/token` with `content-type:
application/json` and a JSON body. The current
[HCP API overview](https://developer.hashicorp.com/hcp/docs/hcp/api) shows
`https://auth.idp.hashicorp.com/oauth2/token` with a form-encoded body. **This
module implements the second**, because it is the first-party API reference
rather than a support-desk article, and because it is the standard RFC 6749
shape. The integration test is what confirms it.

**The read path.** The same support article shows the `2023-06-13` API at
`.../apps/{app}/open/{secret_name}`. The current reference shows `2023-11-28` at
`.../apps/{app_name}/secrets/{secret_name}:open`, a Google AIP custom method.
**This module implements the second.** A version bump here means a different
response shape rather than the same one at a new URL, which is why there is no
`WithAPIVersion` option: pointing this code at `2023-06-13` would send a valid
request and then fail to parse the reply.

### One thing that could not be pinned

**The failure status codes are not documented.** HCP's OpenAPI document
enumerates only `200` and a catch-all `default` (`googlerpcStatus`) for every
operation, so there is no published list of which statuses this endpoint
actually returns. `errors_test.go`'s table is therefore not a transcription of a
vendor list; it is the set of statuses a gRPC-gateway control plane behind a
load balancer produces in practice, each asserted against the kind mamori must
act on. Treat that table as this module's best defensible guess until the
integration test runs against a live account.

## Authentication

An HCP service principal key pair is exchanged for a short-lived access token
through the standard RFC 6749 client-credentials grant.

```bash
export HCP_CLIENT_ID=...
export HCP_CLIENT_SECRET=...
```

An explicit option wins over its environment variable, and the environment is
read lazily at resolve time, so registering this provider from `init` is safe
even when no credentials exist at process start.

### Why this provider *does* use `httpcore.OAuth2ClientCredentials`

`providers/infisical`, the closest sibling, writes its own token authenticator
because Infisical posts JSON `{clientId, clientSecret}` to a vendor-specific
path and answers with `accessToken`/`expiresIn`: nothing about it is RFC 6749.

HCP is the opposite case. Its grant *is* RFC 6749, form-encoded, with the
optional `audience` parameter that `httpcore.OAuth2Config` already supports, and
its reply uses the standard `access_token`/`expires_in`/`token_type` names. So
this provider reuses the shared authenticator and inherits, for free:

- the client secret and the cached access token held in **closures**, out of
  reflection's reach (see "Nothing secret reaches an error" below);
- concurrent exchanges collapsed into one, with each waiter released by its own
  context rather than blocking mamori's single-goroutine reconciler;
- refresh 30s before stated expiry, and a bounded lifetime when the server
  commits to none;
- a token-URL scheme check, because the grant POSTs the client secret.

The `audience` is not optional in practice: HCP issues a token only for
`https://api.hashicorp.cloud`, and the control plane refuses one issued without
it. `TestTokenExchangeMatchesTheGrantHCPDocuments` pins it.

## Value mapping

| `mamori.Value` field | Source |
| --- | --- |
| `Bytes` | `secret.static_version.value`, after `#key` selection |
| `Version` | `secret.static_version.version`, else a content hash |
| `Sensitive` | always `true` |

`Version` is computed over the **whole** secret, not the selected fragment, so
two refs selecting different `#field`s of one JSON secret agree on when it
changed. An absent revision falls back to `mamori.VersionHash` rather than
rendering as `"0"`: a constant version would make change detection impossible
for every ref at once, the one failure a poller cannot report.

### Static secrets only

HCP Vault Secrets also serves **rotating** and **dynamic** secrets, whose values
arrive as a map under `rotating_version.values` or `dynamic_instance.values`
rather than as a single `static_version.value`. This module reads static (`kv`)
secrets only.

A rotating or dynamic secret resolves to a classified error naming the
limitation, **not** to an empty value and **not** to `not_found`: the secret
exists, so reporting it as absent would make mamori apply the field's default
and hide a real configuration mismatch. That shape has not been pinned against a
live backend, and guessing at it is exactly the kind of finished-looking wrong
answer this module refuses to ship.

## Error classification

Every failure is classified through `httpcore.ClassifyStatus`.

| Status | `mamori.Kind` | Effect |
| --- | --- | --- |
| 400, 422 | `invalid` | terminal |
| 401 | `unauthenticated` | terminal |
| 403 | `permission_denied` | terminal |
| 404 | `not_found` | **applies the field's `default:` / `optional`** |
| 408, 429 | `rate_limited` | retried |
| anything else | `unavailable` | retried |

A misconfiguration (no organization, no project, no app, no credentials, a bad
URL scheme) is `invalid`, never `not_found`. That distinction is load bearing:
`not_found` is the one kind that makes mamori apply a default, so reporting a
typo as one would turn a deployment mistake into a silently defaulted secret.

A transient failure at the **identity provider** stays transient. httpcore's
`authError` only adds `ErrUnauthenticated` to an unclassified cause, so a 503
from the token endpoint reports `unavailable` and heals on the next poll rather
than marking the field permanently unhealthy.

### Nothing secret reaches an error

Three separate rules, each with a test that fails when it is broken:

1. **Only the vendor's `message` field** reaches an error, through httpcore's
   `ErrorDetail` hook. Never the whole body, never `details` (an open list of
   arbitrary typed payloads), and never a sibling field. On the success path the
   body *is* the secret.
2. **JSON decode errors are dropped, not wrapped.** `encoding/json` quotes the
   offending byte in a syntax error, and that byte is part of the secret.
3. **No credential lives in a struct field.** `fmt`'s `%v`, `%+v` and `%#v` walk
   unexported fields by reflection, and reflection cannot call a `String` or
   `GoString` method on a value it reaches that way, so a redaction method would
   not save a plain field. The client secret lives in a closure here, and the
   access token lives in one inside httpcore.

The test fake **deliberately echoes the client secret, the access token and the
secret value in its error envelopes**. Without that, "no credential reaches an
error" would pass against a provider that pasted the whole body into its
message. That is what makes the assertion falsifiable, and it is verified by
mutation: making the provider surface the whole body fails those tests.

## No native watch

The OpenAppSecret endpoint exposes no streaming read, no blocking read, and no
ETag to gate a conditional GET on, so this provider deliberately does **not**
implement `mamori.WatchableProvider`. mamori wraps it in the polling adapter and
uses `Value.Version` to detect a change between ticks.
`TestProviderIsNotWatchable` pins that decision so it cannot be undone silently.

## Configuration

```go
import (
    "github.com/xavidop/mamori"
    _ "github.com/xavidop/mamori/providers/hcp-vault-secrets"
)

type Config struct {
    DBPassword secret.String `source:"hcp-vs://DB_PASSWORD"`
}
```

Or explicitly:

```go
import hcpvs "github.com/xavidop/mamori/providers/hcp-vault-secrets"

p := hcpvs.New(
    hcpvs.WithClientID(id),
    hcpvs.WithClientSecret(sec),
    hcpvs.WithOrganizationID(org),
    hcpvs.WithProjectID(proj),
    hcpvs.WithAppName("web"),
)
```

### Options

| Option | Purpose |
| --- | --- |
| `WithClientID` | service principal client id |
| `WithClientSecret` | service principal client secret |
| `WithOrganizationID` | default organization (UUID) |
| `WithProjectID` | default project (UUID) |
| `WithAppName` | default application |
| `WithBaseURL` | override `api.cloud.hashicorp.com` |
| `WithTokenURL` | override `auth.idp.hashicorp.com/oauth2/token` |
| `WithAllowInsecure` | permit `http://`, and nothing else |
| `WithHTTPClient` | custom transport or proxy |

`WithBaseURL` and `WithTokenURL` are separate because HCP genuinely serves the
two from different hosts. A single override would force a test double to
impersonate both, and would send the client secret to whichever host an operator
pointed the API at.

## Testing status

| Behaviour | Status | Pinned by |
| --- | --- | --- |
| Conformance kit | verified | `TestConformance` |
| Read path template, incl. `:open` | documented, not live-verified | `TestResolveBuildsTheDocumentedPath` |
| Token grant shape and audience | documented, not live-verified | `TestTokenExchangeMatchesTheGrantHCPDocuments` |
| Success JSON nesting | documented, not live-verified | `TestResolveBuildsTheDocumentedPath` |
| Status-to-kind mapping | **inferred**, see the gap above | `TestStatusToKind` |
| Token caching | verified against the fake | `TestAccessTokenIsCachedAcrossResolves` |
| No credential in an error | verified, fake echoes them | `TestResolveErrorCarriesNoCredential` |
| No credential via reflection | verified | `TestProviderNeverPrintsACredential` |
| Static-only limitation | verified | `TestNonStaticSecretIsRejected` |
| Live wire shapes | **needs a live backend** | `//go:build integration` |

### Live integration test

```bash
export HCP_CLIENT_ID=...
export HCP_CLIENT_SECRET=...
export HCP_ORGANIZATION_ID=...
export HCP_PROJECT_ID=...
export HCP_APP_NAME=...
export HCP_TEST_SECRET_NAME=SOME_EXISTING_STATIC_SECRET
GOWORK=off go test -tags integration -run Integration ./...
```

All six are required; every test skips cleanly if any is unset. Nothing logs a
client secret, an access token, or a resolved value: only the secret **name**
and a byte count.

## Development

This package is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/hcp-vault-secrets
GOWORK=off go mod tidy
GOWORK=off go vet ./...
GOWORK=off go vet -tags integration ./...
GOWORK=off go test -race ./...
```
