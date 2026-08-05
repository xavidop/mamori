---
layout: ../../../layouts/DocsLayout.astro
title: HCP Vault Secrets provider
---

# HCP Vault Secrets

Load a secret from [HCP Vault Secrets](https://developer.hashicorp.com/hcp/docs/vault-secrets), the hosted secret-management service on HashiCorp Cloud Platform. Pure `net/http` on top of `providers/httpcore`, no third-party SDK.

This is not the [`vault://` provider](/docs/providers/vault). HCP Vault Secrets is HashiCorp's SaaS product: it lives at `api.cloud.hashicorp.com`, authenticates with an HCP service principal, and addresses a secret by organization, project, app and name. Vault you run yourself takes a Vault token and a KV v2 mount path instead, and neither provider can resolve the other's refs.

| | |
| --- | --- |
| Scheme | `hcp-vs://` |
| Module | `github.com/xavidop/mamori/providers/hcp-vault-secrets` |
| Sensitive | yes (always) |
| Watch | poll |
| Auth | `HCP_CLIENT_ID` / `HCP_CLIENT_SECRET` (service principal) |

## Install

```bash
go get github.com/xavidop/mamori/providers/hcp-vault-secrets
```

```go
import _ "github.com/xavidop/mamori/providers/hcp-vault-secrets"
```

## Using the ref

An `hcp-vs://` ref points at one secret in one app, optionally selecting a field from a JSON value stored in it.

```text
hcp-vs://<secretName>
hcp-vs://<secretName>?org=<id>&project=<id>&app=<name>
hcp-vs://<secretName>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secretName>` | yes | The **entire ref path**, including any slashes it contains. The organization, project and app that scope it are never taken from the path. |
| `?org=<id>` | no | Overrides the configured organization for this ref only. |
| `?project=<id>` | no | Overrides the configured project for this ref only. |
| `?app=<name>` | no | Overrides the configured app for this ref only. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey`: a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `hcp-vs://DB_PASSWORD` reads `DB_PASSWORD` from the configured app.
- `hcp-vs://DB_PASSWORD?app=web` reads the same name from the `web` app.
- `hcp-vs://DB_CREDS#password` reads top-level field `password` of a JSON secret.
- `hcp-vs://DB_CREDS#/creds/password` selects a nested field by JSON Pointer.
- `hcp-vs://a%2Fb` reads the single secret named `a/b`.

```go
type Config struct {
	DBPassword secret.String `source:"hcp-vs://DB_PASSWORD"`
	APIKey     secret.String `source:"hcp-vs://API_KEY?app=web"`
}
```

`Value.Bytes` is the secret's static version value, after `#field` selection. `Value.Version` is that static version's number, or a content hash when the backend sends none; it describes the whole secret rather than the selected fragment, so two refs selecting different `#field`s of one secret agree on when it changed. `Value.Sensitive` is always `true`.

## Static secrets only

HCP Vault Secrets also serves **rotating** and **dynamic** secrets, whose values arrive as a map rather than as a single value. This provider reads static (`kv`) secrets only.

A rotating or dynamic secret resolves to an `invalid` error naming the limitation, never to an empty value and never to `not_found`. The secret exists, so reporting it as absent would make mamori apply the field's `default:` and hide a real configuration mismatch.

## Authentication

An HCP service principal key pair is exchanged for a short-lived access token through the RFC 6749 client-credentials grant. Set `HCP_CLIENT_ID` and `HCP_CLIENT_SECRET`, or pass `WithClientID` and `WithClientSecret`; an explicit option wins over its environment variable.

The credentials are read lazily at resolve time, so a blank import is safe even when none exist at process start. The token is cached and refreshed before expiry, so polling costs one request per poll rather than two. Neither the client secret nor the cached token is held in a readable struct field, so neither turns up in a `%v` or `%+v` dump.

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

## Watch

The read endpoint exposes no streaming read, no blocking read, and no ETag, so this provider does not implement a native watch. mamori polls it and uses `Value.Version` to detect a change between ticks. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads across a poll interval.

## Configuration

```go
import hcpvs "github.com/xavidop/mamori/providers/hcp-vault-secrets"

mamori.WithProvider(hcpvs.New(
	hcpvs.WithClientID(id),
	hcpvs.WithClientSecret(sec),
	hcpvs.WithOrganizationID(org),
	hcpvs.WithProjectID(proj),
	hcpvs.WithAppName("web"),
))
```

A ref option wins over a provider option, which wins over the environment variable.

| Scope | Provider option | Ref option | Environment variable |
| --- | --- | --- | --- |
| Organization | `WithOrganizationID` | `?org=` | `HCP_ORGANIZATION_ID` |
| Project | `WithProjectID` | `?project=` | `HCP_PROJECT_ID` |
| App | `WithAppName` | `?app=` | `HCP_APP_NAME` |

For a proxy or a test double, `WithBaseURL` overrides `https://api.cloud.hashicorp.com`, `WithTokenURL` overrides `https://auth.idp.hashicorp.com/oauth2/token`, and `WithHTTPClient` injects an `*http.Client`. An `http://` URL for either endpoint additionally requires `WithAllowInsecure()`, which takes no argument and permits cleartext `http`, nothing else.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting HCP. Without `WithHTTPClient` it releases nothing: this provider builds no default client of its own to hold onto. A client injected with `WithHTTPClient` is never closed: `Close` may return its idle connections to the pool, but leaves the client usable.

The endpoint templates and JSON shapes come from HashiCorp's [API reference](https://developer.hashicorp.com/hcp/api-docs/vault-secrets/openappsecret) rather than from a live call, since nobody on this project has HCP credentials; everything else is verified against an in-process fake. A `//go:build integration` test closes that gap against a real account when `HCP_CLIENT_ID`, `HCP_CLIENT_SECRET`, `HCP_ORGANIZATION_ID`, `HCP_PROJECT_ID`, `HCP_APP_NAME` and `HCP_TEST_SECRET_NAME` are set; it logs only a byte count, never a token or a value.

## See also

- [Vault provider](/docs/providers/vault) for self-hosted HashiCorp Vault
- [Provider catalog](/docs/providers)
