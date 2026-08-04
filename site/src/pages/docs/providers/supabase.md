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

## This provider requires setup on your Supabase project

**Read this before anything else. Without it, this provider cannot work at all.**

Supabase Vault stores secrets in `vault.secrets` and exposes their plaintext through the `vault.decrypted_secrets` view. **That view is not reachable over the Data API, and no header, key, or setting makes it reachable.**

The reason is not that `vault` merely happens to be unexposed by default. An ordinary custom schema can be added to the dashboard's **Exposed schemas** list; `vault` cannot. Supabase [restricts the `auth`, `storage`, `realtime` and `vault` schemas](https://github.com/orgs/supabase/discussions/34270) precisely so third-party tooling cannot reach them. Sending `Accept-Profile: vault` returns PostgREST's `PGRST106`, *"The schema must be one of the following"*, and no amount of granting fixes it.

So this provider reads **a relation you create in a schema that is exposed**, which re-exposes the Vault view under privileges you control.

### The setup SQL

Run this once in your project's SQL editor.

```sql
-- 1. A view in an exposed schema that mirrors vault.decrypted_secrets.
--    security_invoker = on means the view runs with the CALLER's privileges,
--    so the grant below is what actually gates access, rather than the view
--    silently inheriting its owner's rights.
create or replace view public.decrypted_secrets
with (security_invoker = on) as
  select id, name, decrypted_secret, updated_at
  from vault.decrypted_secrets;

-- 2. Revoke from everyone, then grant to service_role alone. The revoke is not
--    redundant: a project created before Supabase stopped auto-exposing new
--    tables may already carry anon/authenticated grants.
revoke all on public.decrypted_secrets from anon, authenticated;
grant select on public.decrypted_secrets to service_role;

-- 3. service_role must also reach the underlying Vault objects, since
--    security_invoker means the caller's own privileges are used.
grant usage on schema vault to service_role;
grant select on vault.decrypted_secrets to service_role;
```

To put the view in a dedicated schema instead, create it there, add that schema to **Exposed schemas** in API settings, apply the [custom-schema grants](https://supabase.com/docs/guides/api/using-custom-schemas), and point the provider at it with `?schema=`.

### You must use the service-role key

| Key | Use it here? | Why |
| --- | --- | --- |
| **service-role** (secret) | **yes** | It is the only key whose role the view above is granted to. |
| **anon** (publishable) | **no** | It is refused by the grant, and *making* it work would mean granting `anon` access to your decrypted secrets. The anon key ships in browsers, so that grant publishes your entire Vault to the internet. |

The service-role key [uses the `BYPASSRLS` attribute, skipping any and all Row Level Security policies](https://supabase.com/docs/guides/api/api-keys). It is not one secret among many; it is the key to every row in the project. Keep it in server-side configuration, never in a browser or a mobile bundle. Nothing in this provider logs it, puts it in an error, or holds it in a readable struct field.

### Checking the setup worked

```bash
curl "$SUPABASE_URL/rest/v1/decrypted_secrets?name=eq.my-secret&select=name,decrypted_secret,updated_at" \
  -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Accept-Profile: public"
```

A working setup answers `200` with a one-element array.

## Using the ref

A `supabase://` ref points at one Vault secret by name, optionally overriding the schema or relation and selecting a field from a JSON-valued secret.

```text
supabase://<secretName>
supabase://<secretName>?schema=<name>
supabase://<secretName>?view=<name>
supabase://<secretName>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secretName>` | yes | The **entire ref path**, matching the `name` column. Slashes and dot segments are ordinary characters here. |
| `?schema=<name>` | no | The exposed schema holding the relation. Defaults to `public`. |
| `?view=<name>` | no | The relation name. Defaults to `decrypted_secrets`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

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

A secret name is a PostgREST filter *value* in the query string, never a path segment, so unlike most providers there is no traversal concern: `supabase://../etc/passwd` is an ordinary name that either matches a row or matches nothing. A name containing a literal `#` cannot be expressed as a ref, because mamori's grammar claims `#` for field selection.

## Fetching a value

One ref is one authenticated request:

```text
GET /rest/v1/decrypted_secrets?name=eq.<name>&select=name,decrypted_secret,updated_at
apikey: <service-role key>
Authorization: Bearer <service-role key>
Accept-Profile: public
```

- **Both credential headers travel, carrying the same key.** Supabase's gateway authenticates `apikey`; PostgREST reads the `Authorization` bearer to choose the database role. Sending only `apikey` reaches PostgREST as the anonymous role no matter which key it holds, so a correct service-role key is still refused the view.
- **`Accept-Profile` is sent always, even for `public`.** PostgREST selects the *first* entry of `db-schemas` as its default, so a project exposing `api,public` would read a different schema than one exposing `public,api`.
- **`select=` names three columns rather than `*`.** The Vault view also carries the raw ciphertext, the key id and the nonce.

The response is a JSON array even for a single row:

```json
[{"name":"db-password","decrypted_secret":"hunter2","updated_at":"2026-08-04T12:00:00.123456+00:00"}]
```

## An absent secret is an empty array, not a 404

**PostgREST answers a filter that matches nothing with `200 OK` and `[]`.** It does not return `404`. A `404` from this endpoint means the *relation* does not exist, which usually means the setup SQL was never applied.

This provider maps that empty array onto `mamori.ErrNotFound`, and that mapping matters more than any other in the package. `ErrNotFound` is the one kind that changes mamori's *behaviour* rather than only its reporting: it is what makes a field's `default:` and `optional` handling apply instead of failing the whole snapshot. Classifying the empty array as anything else, `invalid` included, would turn an absent optional secret into a hard startup failure.

Every *misconfiguration*, by contrast, is `invalid` and explicitly never `not_found`, so a broken setup can never hide behind a silently applied default.

| Rows returned | Result |
| --- | --- |
| exactly 1 | the value |
| 0 (an empty array, status 200) | `not_found`, so a field's `default:` applies |
| more than 1 | `invalid` - your relation is not one-row-per-name |
| 1, but no `decrypted_secret` column | `invalid` - the relation is missing that column |

## Value mapping

| Field | Value |
| --- | --- |
| `Value.Bytes` | `decrypted_secret`, after `#field` selection when the ref asks for one |
| `Value.Version` | `updated_at`, or a content hash when the relation omits that column |
| `Value.Sensitive` | always `true` |

### Why `updated_at` is the Version, and not `id`

`vault.decrypted_secrets` offers two candidates and only one works. **`id` is a UUID identifying *which* secret**, and Vault's `update_secret` rewrites a row in place, so the id is unchanged by a rotation: using it would pin `Version` to a constant for the life of the secret and make change detection impossible, the one failure mode a poller cannot report. **`updated_at` advances on every write**, which is exactly what a `Version` is for.

It also beats a content hash, the fallback: it distinguishes a rewrite of identical bytes from no write at all. The hash fallback exists because your relation may omit the column, and rendering an absent timestamp as `""` would pin `Version` to a constant for every ref at once.

The version describes the whole secret, not the selected fragment, so two refs selecting different `#field`s of one secret agree on when it changed.

## Error classification

Status classification is shared with every other REST-backed provider via `httpcore.ClassifyStatus`:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else (402, 405, 406, 415, 416, 500, 503, 504, ...) | `unavailable` |

What each means for a Supabase project:

| You see | PostgREST code | Almost certainly |
| --- | --- | --- |
| `200` with `[]` | - | No secret of that name. A field's `default:` applies. |
| `404` | `PGRST205` | The relation does not exist in that schema: the setup SQL was not applied, or `?view=` names something else. |
| `406` | `PGRST106` | The schema is not in **Exposed schemas**. Naming `vault` always lands here. |
| `401` | `PGRST301` | Missing or malformed key, or only one of the two headers arrived. |
| `403` | `42501` | The key's role lacks `select` on the relation. Usually an anon key, or the grant was skipped. |

### Nothing secret reaches an error

PostgREST's error envelope has four fields, and only **`message`** and **`code`** are surfaced. **`details` is excluded** because PostgREST documents it as carrying the offending row, and for this provider a row *is* the decrypted secret. **`hint` is excluded** because it is free-form text with no guarantee bounding what it quotes. A JSON decode failure drops its cause, since a 200 body here contains the secret. The service-role key travels in headers rather than a query parameter, and lives in a closure rather than a struct field, so it cannot appear in a URL, an error, or a `%+v` dump.

The in-process test fake's error envelope deliberately echoes both the key and the secret value, so those assertions can actually fail.

## Configuration

```go
import "github.com/xavidop/mamori/providers/supabase"

p := supabase.New(
	supabase.WithProjectURL(os.Getenv("SUPABASE_URL")),
	supabase.WithServiceKey(os.Getenv("SUPABASE_SERVICE_ROLE_KEY")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

A ref option wins over a provider option, which wins over the environment variable, which wins over the default - the same rule as [Cloudflare KV](/docs/providers/cloudflare-kv)'s `?namespace=`.

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

### Local development

`supabase start` serves the Data API from `http://127.0.0.1:54321`, which requires `WithAllowInsecure()`. It takes no argument deliberately: every request carries a key that bypasses row-level security on the whole project, so an opt-in that cannot be switched on by a `bool` that happened to default to `true` is the safer shape. It permits cleartext `http` and nothing else, never a way to skip the scheme check itself.

## No native watch

PostgREST exposes no streaming read, no blocking read, and no ETag to gate a conditional GET on, so this provider deliberately does not implement `mamori.WatchableProvider`. mamori wraps it in the polling adapter and uses `Value.Version` (the row's `updated_at`) to detect a change between ticks. To avoid a round trip on every tick, compose `middleware.Cache` in front of it.

Supabase Realtime cannot help here: it publishes from the WAL for tables you add to a publication, and the `vault` schema is restricted from exactly that kind of access.

## Testing status

Wire shapes are pinned from vendor documentation on 2026-08-04. **Nobody on this project has a Supabase project**, so the request and response shapes are documented rather than live-verified. The conformance suite, value mapping, error classification, precedence and credential hygiene all run against an in-process fake and are verified; the wire shapes and the empty-array behaviour are taken from the vendor's reference. A build-tagged integration test closes the gap for anyone with a project:

```bash
export SUPABASE_URL=https://<project-ref>.supabase.co
export SUPABASE_SERVICE_ROLE_KEY=...
export SUPABASE_TEST_SECRET_NAME=some-existing-vault-secret
GOWORK=off go test -tags integration -run Integration ./...
```

It never logs the key or a resolved value: only the secret name, a byte count, and a version. One of its cases asserts that reading `vault.decrypted_secrets` **directly** still fails; if that ever starts passing, Supabase has relaxed the restriction and the setup requirement can be dropped.

The full source list is in the [module README](https://github.com/xavidop/mamori/tree/main/providers/supabase#testing-status).
