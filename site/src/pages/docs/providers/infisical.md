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
| `<secretName>` | yes | The **entire ref path**, including any slashes it contains. The project, environment and folder are never taken from the path: a segment-count rule would silently misread one name containing a slash as two. |
| `?project=<id>` | no | Overrides the configured project for this ref only. |
| `?env=<slug>` | no | Overrides the configured environment (`dev`, `prod`, ...) for this ref only. |
| `?path=/folder` | no | Overrides the configured folder for this ref only. Defaults to `/`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

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

A secret name containing a literal `#` cannot be expressed as a ref: mamori's grammar parses the `#key` fragment before the `?opts` query, claiming `#` for field selection, so there is no way to escape one into a path. A name containing a `.` or `..` path segment is refused with `mamori.ErrInvalid`, because `httpcore` rejects a dot segment on the decoded path for every provider built on it. A name containing `%2e%2e` resolves normally: the whole name is escaped, so it travels as a single segment and is a literal name rather than a traversal.

## Value mapping

`Value.Bytes` is the secret's `secretValue`, after `#field` selection when the ref asks for one. `Value.Version` is the backend's own `version` rendered as a string, so mamori gets change detection from Infisical rather than from a content hash; a content hash is the fallback when the backend sends no revision, because rendering an absent one as `"0"` would pin `Version` to a constant and make change detection impossible for every ref at once. The version describes the whole secret, so two refs selecting different `#field`s of one secret agree on when it changed.

`Value.Sensitive` is always `true`, with no per-ref or per-provider switch. This is a secret manager; there is no configuration-only mode of Infisical for a flag to describe.

**The read path is `v4`**, not the `v3` most third-party write-ups still describe. There is deliberately no `WithAPIVersion` option: a second version means a second response shape, and guessing at one nobody has tested would be worse than requiring a provider update.

## Authentication

Machine identity **Universal Auth**: a client id and client secret are exchanged for a short-lived access token, cached and refreshed 30 seconds before its stated expiry so a request is never sent with a token that dies in flight. Supply them with `WithClientID`/`WithClientSecret` or through `INFISICAL_CLIENT_ID`/`INFISICAL_CLIENT_SECRET`; an explicit option wins over its environment variable, and the environment is read lazily at resolve time so a blank import alone is enough once the environment is set.

The access token is cached across resolves; the **value** never is. `mamori.Refresh` and `mamori.Doctor` both call `Resolve` directly, and Infisical exposes no ETag or digest to gate a held snapshot on.

### Why not `httpcore.OAuth2ClientCredentials`

It does not fit, in both directions. The RFC 6749 client-credentials grant posts form-encoded `grant_type`/`client_id`/`client_secret` to a configured token endpoint and reads `access_token`/`expires_in`; Infisical posts JSON `{"clientId":..., "clientSecret":...}` to `/api/v1/auth/universal-auth/login` and answers with `accessToken`/`expiresIn`. So this provider writes its own token authenticator, following `httpcore`'s structure decision for decision: the lock is never held across the login round trip, concurrent callers share one exchange, a waiter is released by its own context, and no credential is held in a readable struct field.

That last point is worth spelling out. `fmt`'s `%+v` and `%#v` walk unexported fields by reflection, and reflection cannot call a `String` or `GoString` method on a value it reaches that way, so a redaction method would not have protected either the client secret or the access token: `fmt` falls back to printing the raw contents. Both live inside closures, which reflection renders as bare function pointers, and the `Provider` itself gets the same treatment because it is exactly the value an application passes to `mamori.WithProvider`.

## Self-hosted installs

`WithBaseURL(u)` overrides `https://app.infisical.com`. The scheme is checked against a closed set of `https` and `http`, so an `ftp://` typo or a `ws://` paste fails once rather than on every resolve with `net/http`'s "unsupported protocol scheme". An `http://` base URL additionally requires `WithAllowInsecure()`, because Universal Auth POSTs the client secret in the request body and that secret is the key to every value the backend serves. `WithAllowInsecure()` takes no argument on purpose, and permits cleartext `http` and nothing else.

Both checks run on the first `Resolve` rather than in `New`, which returns no error so a blank import can register the scheme from `init`. `mamori doctor` resolves every ref before deployment, so a misconfiguration still surfaces before production.

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

**404 is `not_found`, and nothing else** - that is the one kind that changes mamori's behaviour rather than only its reporting, since it makes a field's `default:` and `optional` handling apply. A misconfiguration must therefore never be reported as `not_found`, which is why a missing project id, an empty secret name and a malformed 200 response all wrap `mamori.ErrInvalid`.

**422 is `invalid`, not `unavailable`.** Infisical is the backend in this ecosystem that answers `422 Unprocessable Entity`, and the default kind is transient: mamori would back off and retry a request that was well formed and semantically wrong. `httpcore` names 422 explicitly for exactly this provider.

A failure on the login leg keeps its own kind rather than being flattened to `unauthenticated`, so a passing blip at Infisical's identity endpoint reports `unavailable` (which mamori expects to heal) rather than a terminal credential failure.

Nothing secret reaches an error. Only the vendor's `message` field is surfaced, never the whole body, because a response body can be the resolved secret itself; a `message` that is not a string suppresses the detail rather than being guessed at. The login response never reaches an error at all, because it is the one reply to a request that contained the client secret. A JSON decode failure drops its cause, because `encoding/json` quotes the offending byte. The access token travels in an `Authorization` header, never a query parameter.

## Watch

The Infisical read API exposes no streaming read, no blocking read, and no ETag to gate a conditional GET on, so unlike the generic [HTTPS provider](/docs/providers/https) there is no `httpcore.Revalidator` here: every poll is a full read. This provider does not implement `WatchableProvider`; mamori polls it instead (`WithPollInterval` + jitter), using the backend revision in `Version` to detect a change between ticks. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads across a poll interval.

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

A ref option wins over a provider option, which wins over the environment variable - exactly the [Cloudflare Workers KV](/docs/providers/cloudflare-kv) provider's `?namespace=` rule.

| Scope | Provider option | Ref option | Environment variable | Default |
| --- | --- | --- | --- | --- |
| Project id | `WithProjectID` | `?project=` | `INFISICAL_PROJECT_ID` | none, **required** |
| Environment | `WithEnvironment` | `?env=` | `INFISICAL_ENVIRONMENT` | omitted from the request |
| Secret path | `WithSecretPath` | `?path=` | `INFISICAL_SECRET_PATH` | `/` |

An unconfigured environment is omitted from the request rather than sent empty, because the API treats it as optional and "absent" is not the same request as "present and empty".

| Option | Effect |
| --- | --- |
| `WithClientID(id)` | Machine identity client id; empty falls back to the environment |
| `WithClientSecret(secret)` | Machine identity client secret, captured in a closure rather than a field; empty falls back to the environment |
| `WithProjectID(id)` | Default project id |
| `WithEnvironment(slug)` | Default environment slug |
| `WithSecretPath(path)` | Default folder |
| `WithBaseURL(u)` | Override `https://app.infisical.com` for a self-hosted install |
| `WithAllowInsecure()` | Permit an `http://` base URL, and nothing else |
| `WithHTTPClient(c)` | Inject a custom `*http.Client` for both the login and the read |

## Testing status

Wire shapes are pinned from Infisical's own API reference on 2026-08-04: the [read endpoint](https://infisical.com/docs/api-reference/endpoints/secrets/read) and the [Universal Auth login endpoint](https://infisical.com/docs/api-reference/endpoints/universal-auth/login). Nobody on this project has Infisical credentials, so the request and response shapes are **documented, not live-verified**: they come from that reference rather than from a live call, and this page does not claim more.

Everything else is verified against an in-process fake `http.RoundTripper` - value mapping, scope and credential precedence, name escaping, error classification for every status, and that no secret value, access token or client secret reaches an error even when the backend echoes them in its error envelope. The conformance kit therefore runs with no network access and no Infisical account. A `//go:build integration` test closes the remaining gap against a real instance when `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`, `INFISICAL_PROJECT_ID` and `INFISICAL_TEST_SECRET_NAME` are set; it logs only a secret name and a byte count. See the [module README](https://github.com/xavidop/mamori/tree/main/providers/infisical) for the full per-aspect table.
