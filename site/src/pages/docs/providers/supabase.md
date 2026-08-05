---
layout: ../../../layouts/DocsLayout.astro
title: Supabase Vault
---

# Supabase Vault

Load a secret from [Supabase Vault](https://supabase.com/docs/guides/database/vault), the encrypted secret store built into every Supabase Postgres database, read over the project's PostgREST Data API. Pure `net/http` on top of [`providers/httpcore`](/docs/providers/https), no Supabase SDK and no Postgres driver.

| | |
| --- | --- |
| Scheme | `supabase://` |
| Module | `github.com/xavidop/mamori/providers/supabase` |
| Sensitive | yes |
| Watch | poll |
| Auth | `SUPABASE_URL`, `SUPABASE_SERVICE_ROLE_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/supabase
```

```go
import _ "github.com/xavidop/mamori/providers/supabase"
```

## Set up your Supabase project first

**Without this, the provider cannot work at all.**

Supabase Vault keeps secrets in `vault.secrets` and exposes their plaintext through the `vault.decrypted_secrets` view, but [Supabase restricts the `vault` schema](https://github.com/orgs/supabase/discussions/34270) from the Data API: it cannot be added to **Exposed schemas**, and no header, key or setting reaches it. So you create a relation in a schema that *is* exposed, and the provider reads that.

Run this once in your project's SQL editor.

```sql
-- 1. A view in an exposed schema that mirrors vault.decrypted_secrets.
--    security_invoker = on runs the view with the CALLER's privileges, so the
--    grant below is what gates access.
create or replace view public.decrypted_secrets
with (security_invoker = on) as
  select id, name, decrypted_secret, updated_at
  from vault.decrypted_secrets;

-- 2. Revoke from everyone, then grant to service_role alone. A project created
--    before Supabase stopped auto-exposing new tables may already carry
--    anon/authenticated grants, so the revoke is not redundant.
revoke all on public.decrypted_secrets from anon, authenticated;
grant select on public.decrypted_secrets to service_role;

-- 3. security_invoker means the caller needs the underlying objects too.
grant usage on schema vault to service_role;
grant select on vault.decrypted_secrets to service_role;
```

To put the view in a dedicated schema instead, create it there, add that schema to **Exposed schemas** in API settings, apply the [custom-schema grants](https://supabase.com/docs/guides/api/using-custom-schemas), and point the provider at it with `?schema=`.

**Use the service-role key, never the anon key.** The service-role key is the only one whose role the view above is granted to. Making the anon key work would mean granting `anon` access to your decrypted secrets, and the anon key ships in browsers, so that grant publishes your whole Vault to the internet. The service-role key [carries `BYPASSRLS`](https://supabase.com/docs/guides/api/api-keys) and skips every Row Level Security policy, so keep it in server-side configuration only.

Check the setup with a request the provider itself would make. A working setup answers `200` with a one-element array.

```bash
curl "$SUPABASE_URL/rest/v1/decrypted_secrets?name=eq.my-secret&select=name,decrypted_secret,updated_at" \
  -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Accept-Profile: public"
```

## Using the ref

A `supabase://` ref points at one Vault secret by name, optionally overriding the schema or relation and selecting a field from a JSON-valued secret.

```text
supabase://<secretName>
supabase://<secretName>?schema=<name>&view=<name>
supabase://<secretName>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secretName>` | yes | The **entire ref path**, matching the `name` column. Slashes and dot segments are ordinary characters here. |
| `?schema=<name>` | no | The exposed schema holding the relation. Defaults to `public`. |
| `?view=<name>` | no | The relation name. Defaults to `decrypted_secrets`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey`: a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `supabase://db-password` reads `db-password` from `public.decrypted_secrets`.
- `supabase://db-password?schema=api` reads the same secret from a relation in the `api` schema.
- `supabase://api-creds#stripe` reads field `stripe` of the JSON-valued secret `api-creds`.
- `supabase://api-creds#/stripe/secretKey` selects a nested field by JSON Pointer.

```go
type Config struct {
	DBPassword secret.String `source:"supabase://db-password"`
	StripeKey  secret.String `source:"supabase://api-creds#/stripe/secretKey"`
}
```

A secret name travels as a filter value in the query string rather than as a path segment, so `supabase://../etc/passwd` is an ordinary name that either matches a row or matches nothing. A name containing a literal `#` cannot be expressed as a ref, because mamori's grammar claims `#` for field selection.

`Value.Bytes` is the row's `decrypted_secret`, after `#field` selection. `Value.Version` is its `updated_at`, which advances on every write, or a content hash when your relation omits that column; it describes the whole secret, so two refs selecting different `#field`s agree on when it changed. `Value.Sensitive` is always `true`.

## An absent secret is an empty array, not a 404

**PostgREST answers a filter that matches nothing with `200 OK` and `[]`.** It does not return `404`. A `404` from this endpoint means the *relation* does not exist, which usually means the setup SQL was never applied.

| Rows returned | Result |
| --- | --- |
| exactly 1 | the value |
| 0 (an empty array, status 200) | `not_found`, so a field's `default:` applies |
| more than 1 | `invalid`: your relation is not one-row-per-name |
| 1, but no `decrypted_secret` column | `invalid`: the relation is missing that column |

`not_found` is the one kind that makes a field's `default:` and `optional` handling apply, so every misconfiguration above is `invalid` instead: a broken setup can never hide behind a silently applied default.

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

What each means for a Supabase project:

| You see | PostgREST code | Almost certainly |
| --- | --- | --- |
| `200` with `[]` | - | No secret of that name. A field's `default:` applies. |
| `404` | `PGRST205` | The relation does not exist in that schema: the setup SQL was not applied, or `?view=` names something else. |
| `406` | `PGRST106` | The schema is not in **Exposed schemas**. Naming `vault` always lands here. |
| `401` | `PGRST301` | Missing or malformed key, or only one of the two auth headers arrived. |
| `403` | `42501` | The key's role lacks `select` on the relation. Usually an anon key, or the grant was skipped. |

Nothing secret reaches an error. Only PostgREST's `message` and `code` are surfaced, never its `details` or `hint`, which can quote the offending row, and the row here is the decrypted secret. The service-role key travels in headers rather than a query parameter, and lives in a closure rather than a struct field, so it cannot appear in a URL, an error, or a `%+v` dump.

## Watch

PostgREST exposes no streaming read, no blocking read, and no ETag to gate a conditional GET on, so this provider does not implement `mamori.WatchableProvider`. mamori polls it instead (`WithPollInterval` + jitter) and uses `Value.Version`, the row's `updated_at`, to detect a change between ticks. Compose [`middleware.Cache`](/docs/middleware/) in front of it to avoid a round trip on every tick. Supabase Realtime cannot help here: it publishes from the WAL for tables you add to a publication, and the `vault` schema is restricted from that too.

## Configuration

```go
import "github.com/xavidop/mamori/providers/supabase"

p := supabase.New(
	supabase.WithProjectURL(os.Getenv("SUPABASE_URL")),
	supabase.WithServiceKey(os.Getenv("SUPABASE_SERVICE_ROLE_KEY")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

A ref option wins over a provider option, which wins over the environment variable, which wins over the default, the same rule as [Cloudflare KV](/docs/providers/cloudflare-kv)'s `?namespace=`.

| Scope | Provider option | Ref option | Environment variable | Default |
| --- | --- | --- | --- | --- |
| Project URL | `WithProjectURL` | - | `SUPABASE_URL` | none, **required** |
| Service key | `WithServiceKey` | - | `SUPABASE_SERVICE_ROLE_KEY` | none, **required** |
| Schema | `WithSchema` | `?schema=` | `SUPABASE_VAULT_SCHEMA` | `public` |
| Relation | `WithView` | `?view=` | `SUPABASE_VAULT_VIEW` | `decrypted_secrets` |

The environment is read lazily at the first resolve, so a blank import is safe even when no credentials exist at process start.

| Option | Effect |
| --- | --- |
| `WithProjectURL(u)` | Set the `https://<project-ref>.supabase.co` origin; `/rest/v1` is appended for you |
| `WithServiceKey(key)` | Set the service-role key, captured in a closure rather than a field |
| `WithSchema(name)` | Set the exposed schema for refs with no `?schema=` |
| `WithView(name)` | Set the relation for refs with no `?view=`; a name with a slash is rejected |
| `WithAllowInsecure()` | Permit an `http://` project URL, and nothing else |
| `WithHTTPClient(c)` | Inject a custom `*http.Client`; a nil client is a no-op |

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Supabase. Without `WithHTTPClient` it releases nothing: this provider builds no default client of its own to hold onto. A client injected with `WithHTTPClient` is never closed or invalidated, only its idle connections are returned to the pool, and only when that client's `Transport` is non-nil (a nil `Transport` resolves to the shared `http.DefaultTransport`, which `Close` leaves alone).

`supabase start` serves the Data API from `http://127.0.0.1:54321` for local development, which requires `WithAllowInsecure()`. It takes no argument and permits cleartext `http`, nothing else.

The request and response shapes above are taken from Supabase's documentation rather than from a live call, since nobody on this project has a Supabase project; everything else is verified against an in-process fake. A `//go:build integration` test closes that gap against a real project when `SUPABASE_URL`, `SUPABASE_SERVICE_ROLE_KEY` and `SUPABASE_TEST_SECRET_NAME` are set.
