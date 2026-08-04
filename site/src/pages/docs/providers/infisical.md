---
layout: ../../../layouts/DocsLayout.astro
title: Infisical
---

# Infisical

Load a secret from [Infisical](https://infisical.com), the open-source secret manager. Built on [`providers/httpcore`](/docs/writing-a-provider/httpcore) and the standard library only, with no third-party SDK.

| | |
| --- | --- |
| Scheme | `infisical://` |
| Module | `github.com/xavidop/mamori/providers/infisical` |
| Sensitive | yes, always |
| Watch | poll |
| Auth | `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET` (machine identity Universal Auth) |

## Install

```bash
go get github.com/xavidop/mamori/providers/infisical
```

```go
import _ "github.com/xavidop/mamori/providers/infisical"
```

## Using the ref

An `infisical://` ref points at one secret, optionally selecting a field from a JSON value stored in it.

```text
infisical://<secretName>
infisical://<secretName>?project=<id>&env=<slug>&path=/folder
infisical://<secretName>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secretName>` | yes | The **entire ref path**, including any slashes it contains. The project, environment and folder are never taken from the path. |
| `?project=<id>` | no | Overrides the configured project for this ref only. |
| `?env=<slug>` | no | Overrides the configured environment (`dev`, `prod`, ...) for this ref only. |
| `?path=/folder` | no | Overrides the configured folder for this ref only. Defaults to `/`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey`: a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `infisical://DB_PASSWORD` reads `DB_PASSWORD` from the configured project, environment and folder.
- `infisical://DB_PASSWORD?env=staging` reads the same secret from the `staging` environment.
- `infisical://DB_PASSWORD?path=/backend` reads it from the `/backend` folder.
- `infisical://DB_CREDS#/creds/password` selects a nested field of a JSON-valued secret.

```go
type Config struct {
	DBPassword secret.String `source:"infisical://DB_PASSWORD"`
	APIKey     secret.String `source:"infisical://API_KEY?env=staging"`
	StripeKey  secret.String `source:"infisical://PAYMENTS#/stripe/secretKey"`
}
```

Two rules apply to a secret name.

**A name containing a literal `#` cannot be expressed as a ref.** mamori's grammar claims `#` for field selection, so there is no way to escape one into the path.

**A name cannot contain a `.` or `..` path segment.** Both the plain and the percent-encoded form are rejected with `mamori.ErrInvalid` before any request is sent.

`Value.Bytes` is the secret's `secretValue`, after `#field` selection. `Value.Version` is the backend's own revision number, or a content hash when the backend sends none; it describes the whole secret rather than the selected fragment, so two refs selecting different `#field`s of one secret agree on when it changed. `Value.Sensitive` is always `true`, with no per-ref or per-provider switch.

## Authentication

A machine identity client id and client secret are exchanged for a short-lived access token through Universal Auth. Supply them with `WithClientID` and `WithClientSecret`, or through `INFISICAL_CLIENT_ID` and `INFISICAL_CLIENT_SECRET`; an explicit option wins over its environment variable, and the environment is read lazily at resolve time, so a blank import alone is enough once the environment is set.

The token is cached and refreshed 30 seconds before expiry, so polling costs one request per poll rather than two. The secret value itself is never cached: every poll is a live read.

## Self-hosted installs

`WithBaseURL(u)` overrides `https://app.infisical.com`. An `http://` base URL additionally requires `WithAllowInsecure()`, because Universal Auth posts the client secret in the request body. `WithAllowInsecure()` takes no argument and permits cleartext `http`, nothing else. Any other scheme is rejected outright. Both checks run on the first resolve, so `mamori doctor` surfaces a bad base URL before deployment.

## Error classification

Status classification is `httpcore.ClassifyStatus`, shared with every other REST-backed provider:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

Only a `404` is `not_found`, and `not_found` is the one kind that makes a field's `default:` and `optional` handling apply. A missing project id, an empty secret name and a malformed `200` response are all `invalid`, so a deployment typo fails loudly instead of silently becoming a default.

A failure on the login leg keeps its own kind rather than being flattened to `unauthenticated`, so a passing blip at Infisical's identity endpoint reports `unavailable` and heals on the next poll.

Nothing secret reaches an error message. Only the vendor's `message` field is surfaced, never a whole response body, and the access token travels in an `Authorization` header rather than a query parameter.

## Watch

The Infisical read API exposes no streaming read, no blocking read, and no ETag to gate a conditional GET on, so this provider does not implement `WatchableProvider`. mamori polls it instead (`WithPollInterval` + jitter), using the backend revision in `Version` to detect a change between ticks. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads across a poll interval.

## Configuration

```go
import infisical "github.com/xavidop/mamori/providers/infisical"

mamori.WithProvider(infisical.New(
	infisical.WithClientID(os.Getenv("INFISICAL_CLIENT_ID")),
	infisical.WithClientSecret(os.Getenv("INFISICAL_CLIENT_SECRET")),
	infisical.WithProjectID("abc123"),
	infisical.WithEnvironment("prod"),
))
```

A ref option wins over a provider option, which wins over the environment variable, the same rule as [Cloudflare Workers KV](/docs/providers/cloudflare-kv)'s `?namespace=`.

| Scope | Provider option | Ref option | Environment variable | Default |
| --- | --- | --- | --- | --- |
| Project id | `WithProjectID` | `?project=` | `INFISICAL_PROJECT_ID` | none, **required** |
| Environment | `WithEnvironment` | `?env=` | `INFISICAL_ENVIRONMENT` | omitted from the request |
| Secret path | `WithSecretPath` | `?path=` | `INFISICAL_SECRET_PATH` | `/` |

| Option | Effect |
| --- | --- |
| `WithClientID(id)` | Machine identity client id; empty falls back to the environment |
| `WithClientSecret(secret)` | Machine identity client secret; empty falls back to the environment |
| `WithProjectID(id)` | Default project id |
| `WithEnvironment(slug)` | Default environment slug |
| `WithSecretPath(path)` | Default folder |
| `WithBaseURL(u)` | Override `https://app.infisical.com` for a self-hosted install |
| `WithAllowInsecure()` | Permit an `http://` base URL, and nothing else |
| `WithHTTPClient(c)` | Inject a custom `*http.Client` for both the login and the read |

The wire shapes above come from Infisical's [API reference](https://infisical.com/docs/api-reference/endpoints/secrets/read) rather than from a live call, since nobody on this project has Infisical credentials; everything else is verified against an in-process fake. A `//go:build integration` test closes that gap against a real instance when `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`, `INFISICAL_PROJECT_ID` and `INFISICAL_TEST_SECRET_NAME` are set.
