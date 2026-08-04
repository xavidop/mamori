---
layout: ../../../layouts/DocsLayout.astro
title: HCP Vault Secrets provider
---

# HCP Vault Secrets

Load a secret from [HCP Vault Secrets](https://developer.hashicorp.com/hcp/docs/vault-secrets), the hosted secret-management service on HashiCorp Cloud Platform. Pure `net/http` on top of `providers/httpcore`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `hcp-vs://` |
| Module | `github.com/xavidop/mamori/providers/hcp-vault-secrets` |
| Sensitive | yes (always) |
| Watch | poll |
| Auth | `HCP_CLIENT_ID` / `HCP_CLIENT_SECRET` (service principal) |

## This is not the `vault://` provider

HCP Vault Secrets and self-hosted HashiCorp Vault are different products with different APIs. If you run your own Vault cluster, you want [the Vault provider](/docs/providers/vault) and `vault://`.

| | `vault://` | `hcp-vs://` |
| --- | --- | --- |
| Product | Vault you run (OSS or Enterprise) | HCP Vault Secrets (SaaS) |
| Host | your cluster | `api.cloud.hashicorp.com` |
| Auth | Vault token, `X-Vault-Token` | HCP service principal, OAuth2 bearer |
| Addressing | mount + KV v2 path | organization + project + app + name |

Neither can resolve the other's refs.

## Install

```bash
go get github.com/xavidop/mamori/providers/hcp-vault-secrets
```

```go
import _ "github.com/xavidop/mamori/providers/hcp-vault-secrets"
```

## Scheme

```
hcp-vs://<secretName>[#<key>][?<opts>]
```

The **entire ref path is the secret name**, including any slashes it contains. The organization, project and application that scope that name never come from the path:

| Option | Overrides | Falls back to |
| --- | --- | --- |
| `?org=` | `WithOrganizationID` | `HCP_ORGANIZATION_ID` |
| `?project=` | `WithProjectID` | `HCP_PROJECT_ID` |
| `?app=` | `WithAppName` | `HCP_APP_NAME` |

Precedence is ref option, then provider option, then environment variable.

### Examples

| Ref | Resolves |
| --- | --- |
| `hcp-vs://DB_PASSWORD` | `DB_PASSWORD` in the configured app |
| `hcp-vs://DB_PASSWORD?app=web` | the same name in the `web` app |
| `hcp-vs://DB_CREDS#/creds/password` | a JSON Pointer into a JSON secret |
| `hcp-vs://DB_CREDS#password` | a literal top-level key |
| `hcp-vs://a%2Fb` | the single secret named `a/b` |

## Usage

```go
package main

import (
    "github.com/xavidop/mamori"
    "github.com/xavidop/mamori/secret"
    _ "github.com/xavidop/mamori/providers/hcp-vault-secrets"
)

type Config struct {
    DBPassword secret.String `source:"hcp-vs://DB_PASSWORD"`
    APIKey     secret.String `source:"hcp-vs://API_KEY?app=web"`
}
```

Or configure the provider explicitly:

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

## Authentication

An HCP service principal key pair is exchanged for a short-lived access token through the standard RFC 6749 client-credentials grant, with the `audience` HCP requires:

```bash
export HCP_CLIENT_ID=...
export HCP_CLIENT_SECRET=...
```

The credentials are read lazily at resolve time, so a blank import is safe even when no credentials exist at process start. The token is cached and refreshed before expiry, so mamori's polling costs one request per poll rather than two.

Because the grant is genuinely RFC 6749, this provider reuses `httpcore.OAuth2ClientCredentials` rather than writing its own token authenticator, and inherits its credential hygiene: the client secret and the cached access token are held in closures, out of reach of `fmt`'s reflection-based `%v` / `%+v` / `%#v`.

## Value mapping

| `mamori.Value` field | Source |
| --- | --- |
| `Bytes` | `secret.static_version.value`, after `#key` selection |
| `Version` | `secret.static_version.version`, else a content hash |
| `Sensitive` | always `true` |

`Version` describes the whole secret rather than the selected fragment, so two refs selecting different `#field`s of one JSON secret agree on when it changed.

## Static secrets only

HCP Vault Secrets also serves **rotating** and **dynamic** secrets, whose values arrive as a map rather than as a single value. This provider reads static (`kv`) secrets only.

A rotating or dynamic secret resolves to a classified `invalid` error naming the limitation, not to an empty value and not to `not_found`. The secret exists, so reporting it as absent would make mamori apply the field's `default:` and hide a real configuration mismatch.

## Error classification

| Status | `mamori.Kind` |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

A missing organization, project, app or credential is `invalid`, never `not_found`, so a deployment typo fails loudly instead of silently applying a field's default.

A 503 from the **token endpoint** stays `unavailable` rather than becoming `unauthenticated`, so a passing blip at the identity provider heals on the next poll instead of marking the field permanently unhealthy.

## Watching

The read endpoint exposes no streaming read, no blocking read, and no ETag, so this provider does not implement a native watch. mamori polls it and uses `Value.Version` to detect a change between ticks.

## Documented, not live-verified

Nobody on the mamori project has HCP credentials. The endpoint templates and JSON shapes above come from HashiCorp's API reference rather than from a live call:

- [HCP API overview](https://developer.hashicorp.com/hcp/docs/hcp/api) for the token endpoint, grant and `audience`
- [OpenAppSecret](https://developer.hashicorp.com/hcp/api-docs/vault-secrets/openappsecret) for the read path and response shape

Two vendor sources disagree. HashiCorp's [support article](https://support.hashicorp.com/hc/en-us/articles/18221966166291-HCP-Vault-Secrets-get-create-and-delete-secrets-via-API) documents an older `auth.hashicorp.com/oauth/token` with a JSON body and the `2023-06-13` read path `.../apps/{app}/open/{name}`. This provider implements the current first-party reference instead: `auth.idp.hashicorp.com/oauth2/token` form-encoded, and the `2023-11-28` path `.../apps/{app}/secrets/{name}:open`.

HCP's OpenAPI document also enumerates only `200` and a catch-all `default` error for every operation, so **the failure status codes are not published**. The classification table above is this provider's best defensible mapping rather than a transcription of a vendor list.

The module ships `//go:build integration` tests that confirm all of it against a real account in one command:

```bash
export HCP_CLIENT_ID=... HCP_CLIENT_SECRET=...
export HCP_ORGANIZATION_ID=... HCP_PROJECT_ID=... HCP_APP_NAME=...
export HCP_TEST_SECRET_NAME=SOME_EXISTING_STATIC_SECRET
GOWORK=off go test -tags integration -run Integration ./...
```

They skip cleanly when the variables are unset, and log only a byte count, never a token or a value.

## See also

- [Vault provider](/docs/providers/vault) for self-hosted HashiCorp Vault
- [Provider catalog](/docs/providers)
