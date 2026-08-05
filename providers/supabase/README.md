# mamori - Supabase Vault provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves secrets from
[Supabase Vault](https://supabase.com/docs/guides/database/vault) over a project's
PostgREST Data API. Supabase publishes a Go client, and this repo already ships
[`providers/postgres`](../postgres/) for direct SQL; neither is used here. This is
the REST path, built on [`providers/httpcore`](../httpcore/) and the standard
library only, keeping every SDK and every Postgres driver out of a consumer's
build.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/supabase"
```

---

## ⚠️ This provider requires setup on your Supabase project

**Read this section before anything else. Without it, this provider cannot work
at all.**

Supabase Vault stores secrets in `vault.secrets` and exposes their plaintext
through the `vault.decrypted_secrets` view. **That view is not reachable over the
Data API, and there is no header, key, or setting that makes it reachable.**

The reason is not that `vault` merely happens to be unexposed by default. An
ordinary custom schema can be added to the dashboard's **Exposed schemas** list;
`vault` cannot. Supabase
[restricts the `auth`, `storage`, `realtime` and `vault` schemas](https://github.com/orgs/supabase/discussions/34270)
precisely so that third-party tooling cannot reach them. Sending
`Accept-Profile: vault` gets you PostgREST's `PGRST106`, *"The schema must be one
of the following"*, and no amount of granting fixes it.

So this provider reads **a relation you create in a schema that IS exposed**,
which re-exposes the Vault view under privileges you control.

### The setup SQL

Run this once in your project's SQL editor. It creates a `security_invoker` view
in `public` and grants it to `service_role` **only**.

```sql
-- 1. A view in an exposed schema that mirrors vault.decrypted_secrets.
--    security_invoker = on means the view runs with the CALLER's privileges,
--    so the grant on the next line is what actually gates access, rather than
--    the view silently inheriting its owner's rights.
create or replace view public.decrypted_secrets
with (security_invoker = on) as
  select id, name, decrypted_secret, updated_at
  from vault.decrypted_secrets;

-- 2. Revoke from everyone, then grant to service_role alone.
--    The revoke is not redundant: a project created before Supabase stopped
--    auto-exposing new tables may already carry anon/authenticated grants, and
--    an anon grant on this view publishes every secret in your Vault to the
--    public internet, because the anon key is designed to ship in browsers.
revoke all on public.decrypted_secrets from anon, authenticated;
grant select on public.decrypted_secrets to service_role;

-- 3. service_role must also be able to reach the underlying Vault objects,
--    since security_invoker means the caller's own privileges are used.
grant usage on schema vault to service_role;
grant select on vault.decrypted_secrets to service_role;
```

If you prefer the view in a dedicated schema rather than `public`, create it
there, add that schema to **Exposed schemas** in
[API settings](https://supabase.com/dashboard/project/_/settings/api), apply the
[custom-schema grants](https://supabase.com/docs/guides/api/using-custom-schemas),
and point the provider at it with `WithSchema("your_schema")` or
`?schema=your_schema`.

### 🔑 You must use the service-role key, and it must never reach a client

| Key | Use it here? | Why |
| --- | --- | --- |
| **service-role** (secret) | **Yes** | It is the only key whose role the view above is granted to. |
| **anon** (publishable) | **No** | It is refused by the grant, and *making* it work would mean granting `anon` access to your decrypted secrets. The anon key is designed to be public and ships in browsers, so that grant publishes your entire Vault to the internet. |

The service-role key
[uses the `BYPASSRLS` attribute, skipping any and all Row Level Security policies](https://supabase.com/docs/guides/api/api-keys).
It is not one secret among many: it is the key to every row in your project. Keep
it in server-side configuration, never in a browser, a mobile bundle, or a public
repository. Supabase's own guidance is to
**"Never expose your secret keys publicly"** and **"Never use in a browser, even
on `localhost`"**.

Nothing in this package logs that key, puts it in an error, or holds it in a
readable struct field; see [Nothing secret reaches an error](#nothing-secret-reaches-an-error).

### Checking the setup worked

```sh
curl "$SUPABASE_URL/rest/v1/decrypted_secrets?name=eq.my-secret&select=name,decrypted_secret,updated_at" \
  -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Accept-Profile: public"
```

A working setup answers `200` with a one-element array. The failure modes are
listed under [Error classification](#error-classification).

---

## Scheme

```
supabase://<secretName>                     secret from the configured relation
supabase://<secretName>?schema=<name>       explicit exposed schema
supabase://<secretName>?view=<name>         explicit relation
supabase://<secretName>#field               select a field of a JSON-valued secret
supabase://<secretName>#/json/pointer       select a nested field by RFC 6901 JSON Pointer
```

- `<secretName>` - the **entire ref path**, matching the `name` column. It may
  contain slashes and dot segments freely.

  **This provider is unusual in being able to promise that.** The name is a
  PostgREST filter *value* in the query string, never a path segment, so
  `httpcore`'s dot-segment rejection never applies to it and there is no
  traversal to worry about. `supabase://../etc/passwd` is an ordinary name that
  either matches a row or matches nothing. Compare
  [`providers/infisical`](../infisical/), where the name *is* the path and a
  `..` segment is therefore refused.
- `?schema=`, `?view=` - optional, each overriding the corresponding provider
  option for this ref only, so one provider can serve refs pointing at different
  relations.
- `#field` / `#/json/pointer` - optional. The secret value is parsed as JSON and
  the field selected via `mamori.SelectKey`, identically to every other mamori
  provider: a fragment starting with `/` is an RFC 6901 JSON Pointer
  (`#/creds/password`); anything else is a literal top-level key (`#password`).

**A secret name containing a literal `#` cannot be expressed as a ref.** mamori's
ref grammar parses the `#key` fragment before the `?opts` query, claiming `#` for
field selection. That is not something this provider imposes and there is no
escape hatch around it.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `supabase://db-password` | Secret `db-password` from the configured schema and relation |
| `supabase://db-password?schema=api` | The same secret from a relation in the `api` schema |
| `supabase://db-password?view=my_secrets` | The same secret from a differently-named relation |
| `supabase://api-creds#stripe` | Field `stripe` of the JSON-valued secret `api-creds` |
| `supabase://api-creds#/stripe/secretKey` | A nested field, selected by JSON Pointer |

```go
type Config struct {
    DBPassword secret.String `source:"supabase://db-password"`
    StripeKey  secret.String `source:"supabase://api-creds#/stripe/secretKey"`
}
```

## Fetching a value

One ref is one authenticated `GET`:

```
GET /rest/v1/decrypted_secrets?name=eq.<name>&select=name,decrypted_secret,updated_at
apikey: <service-role key>
Authorization: Bearer <service-role key>
Accept-Profile: public
```

Four things about that request are decisions rather than defaults:

- **Both credential headers travel, carrying the same key.** Supabase's gateway
  authenticates `apikey`; PostgREST reads the `Authorization` bearer to choose
  the database role. Sending only `apikey` is the failure that looks like a
  permissions bug: the request reaches PostgREST as the anonymous role no matter
  which key it holds, so a correct service-role key is still refused the view.
- **`Accept-Profile` is sent always, even for `public`.** PostgREST selects the
  *first* entry of `db-schemas` as its default, so a project exposing
  `api,public` would read a different schema than one exposing `public,api`.
  Naming the schema on every request is what makes a ref mean the same thing on
  both.
- **`select=` names three columns rather than defaulting to `*`.** The Vault view
  also carries the raw ciphertext, the key id and the nonce, and none of them
  belongs in a response this process holds in memory when it needs one column.
- **No `limit=` is sent.** A duplicated name has to stay detectable: see the
  row-count rule below.

**The response is a JSON array even for a single row**, which is PostgREST's
shape for any filtered select:

```json
[{"name":"db-password","decrypted_secret":"hunter2","updated_at":"2026-08-04T12:00:00.123456+00:00"}]
```

There is **no value cache**. `mamori.Refresh` and `mamori.Doctor` both call
`Resolve` directly, and PostgREST exposes no ETag to gate a held snapshot on, so
every call is a live read.

### Row counts carry meaning

| Rows | Result | Why |
| --- | --- | --- |
| exactly 1 | the value | The normal case. |
| **0 (an empty array, with status 200)** | **`mamori.ErrNotFound`** | See below. This is the single most consequential mapping in the package. |
| more than 1 | `mamori.ErrInvalid` | `vault.secrets` keeps `name` unique, so this means your relation is not the one-row-per-name view above. Taking `rows[0]` would resolve a field to whichever row the planner happened to return first. |
| 1, but no `decrypted_secret` column | `mamori.ErrInvalid` | A relation built without that column. The row exists, so applying the field's `default:` would hide a broken relation behind a value that looks deliberate. |

## An absent secret is an empty array, not a 404

**PostgREST answers a filter that matches nothing with `200 OK` and `[]`.** It
does not return `404`. A `404` from this endpoint means the *relation* does not
exist, which is a different fault entirely and usually means the setup SQL was
never applied.

This provider maps that empty array onto `mamori.ErrNotFound`, and that mapping
carries more weight than any other line in the package. `ErrNotFound` is the one
kind that changes mamori's *behaviour* rather than only its reporting: it is what
makes a field's `default:` and `optional` handling apply instead of failing the
whole snapshot. Classifying the empty array as anything else, `ErrInvalid`
included, turns an absent optional secret into a hard startup failure.

That is why the tests assert the exact kind rather than merely "an error
occurred" (`TestEmptyArrayIsNotFoundAndNothingElse` excludes every other sentinel
by name, and `TestEmptyArrayAppliesTheFieldDefault` observes it through
`mamori.Load` on a field that declares a default). It is also why every
*misconfiguration* above is `ErrInvalid` and explicitly never `ErrNotFound`
(`TestNotFoundIsNotReportedForAMisconfiguration`).

## Value mapping

| Field | Value |
| --- | --- |
| `Value.Bytes` | `decrypted_secret`, after `#field` / `#/json/pointer` selection when the ref asks for one |
| `Value.Version` | `updated_at`, or a content hash (`mamori.VersionHash`) when the relation omits that column |
| `Value.Sensitive` | Always `true` |

### Why `updated_at` is the Version, and not `id`

`vault.decrypted_secrets` offers two candidates, and only one of them works.

- **`id` is a UUID identifying *which* secret.** Vault's `update_secret` rewrites
  a row in place, so the id is **unchanged by a rotation**. Using it would pin
  `Version` to a constant for the life of the secret and make change detection
  impossible, which is the one failure mode a poller cannot report: nothing ever
  looks changed, so nothing ever complains.
- **`updated_at` is a microsecond-resolution timestamp that advances on every
  write.** It identifies *when* the secret last changed, which is exactly what a
  `Version` is for.

Nothing is gained by combining the two. The ref already pins the name, and a
secret deleted and recreated under that name gets a later `updated_at` anyway, so
the id would never break a tie `updated_at` had not already broken.

`updated_at` also beats a content hash, which is the fallback: it distinguishes a
rewrite of identical bytes from no write at all
(`TestResolveVersionChangesWhenOnlyTheTimestampDoes` pins exactly that case). The
hash fallback exists because your relation may omit the column, and rendering an
absent timestamp as `""` would pin `Version` to a constant for every ref at once.

**The version describes the whole secret, not the selected fragment**, so two
refs selecting different `#field`s of one secret agree on when it changed.

**`Value.Sensitive` is always `true`, with no switch.** A relation that
re-exposes `vault.decrypted_secrets` has nothing non-secret in it, so there is
nothing for a per-ref flag to describe.

## Error classification

Status classification is `httpcore.ClassifyStatus`, shared with every other
REST-backed provider, so this table is not re-derived here and cannot drift from
the one the conformance `Fail` hook inverts:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else (402, 405, 406, 415, 416, 500, 503, 504, ...) | `unavailable` |

What each one means for a Supabase project specifically:

| You see | PostgREST code | Almost certainly |
| --- | --- | --- |
| `200` with `[]` | - | No secret of that name. `ErrNotFound`, so a field's `default:` applies. |
| `404` | `PGRST205` | The relation does not exist in that schema: the setup SQL was not applied, or `?view=` names something else. |
| `406` | `PGRST106` | The schema is not in **Exposed schemas**. Naming `vault` always lands here. |
| `401` | `PGRST301` | Missing or malformed key, or only one of the two headers reached the server. |
| `403` | `42501` | The key's role lacks `select` on the relation. Usually an anon key, or the grant was skipped. |

**The empty array is not a status code at all**, which is why it has its own test
alongside the status table rather than living inside it.

### Nothing secret reaches an error

- **Only PostgREST's `message` and `code` are surfaced.** The error envelope has
  four fields, and the other two are deliberately never read:
  - **`details` is excluded.** PostgREST documents it as carrying the offending
    row, rendered as `Failing row contains (...)`, and for this provider a row
    *is* the decrypted secret.
  - **`hint` is excluded.** It is free-form text Postgres composes from the
    failing statement, with no vendor guarantee bounding what it can quote, and
    `message` plus `code` already say what went wrong.

  This is exactly why `httpcore` leaves its `ErrorDetail` hook nil by default and
  makes each provider decide.
- **A JSON decode failure drops its cause.** `encoding/json` quotes the offending
  byte in a syntax error, and a 200 body here contains the decrypted secret. The
  secret name and the status are enough to act on.
- **The service-role key travels in headers, never a query parameter**, so
  `httpcore`'s URL redaction has nothing to catch on this path and no request URL
  in an error can carry it.
- **Neither the key nor a value appears in `%v`, `%+v` or `%#v` of a
  `Provider`.** The key lives in a closure rather than a struct field: `fmt`
  walks unexported fields by reflection, and reflection cannot call a `String` or
  `GoString` method on a value it reaches that way, so a redaction method would
  not have helped. A closure renders as an opaque function pointer.

These are not assertions taken on trust. The in-process fake's error envelope
**deliberately echoes both the service-role key and the secret value** in its
`details` and `hint` fields, so a provider that surfaced the whole body, or that
read either field, fails `TestResolveErrorCarriesNoServiceKey` and
`TestResolveErrorCarriesNoSecretValue`. A credential-hygiene test whose fake
cannot produce a leak asserts nothing.

## No native watch

PostgREST exposes no streaming read, no blocking read, and no ETag this provider
could gate a conditional GET on, so unlike [`providers/https`](../https/) there is
no `httpcore.Revalidator` here: every poll is a full read. This provider
deliberately does not implement `mamori.WatchableProvider`, and mamori wraps it in
the polling adapter instead, using `Value.Version` (the row's `updated_at`) to
detect a change between ticks. Pinned by `TestProviderIsNotWatchable`.

If many refs share a poll interval and you want to avoid a round trip on every
tick, compose [`middleware.Cache`](../../middleware/) in front of this provider to
coalesce reads over a TTL you choose.

Supabase Realtime can stream changes to a table, but not to `vault.secrets`:
Realtime publishes from the WAL for tables you add to a publication, and the
`vault` schema is restricted from exactly that kind of access. A future
Realtime-backed watch would therefore have to observe your re-exposed relation,
which is a different design and is not attempted here.

## Configuration

```go
import "github.com/xavidop/mamori/providers/supabase"

p := supabase.New(
    supabase.WithProjectURL(os.Getenv("SUPABASE_URL")),
    supabase.WithServiceKey(os.Getenv("SUPABASE_SERVICE_ROLE_KEY")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

### Scope and its precedence

A ref option wins over a provider option, which wins over the environment
variable, which wins over the default. That is exactly
[`providers/cloudflare-kv`](../cloudflare-kv/)'s `?namespace=` rule, so an
operator who knows one knows the other.

| Scope | Provider option | Ref option | Environment variable | Default |
| --- | --- | --- | --- | --- |
| Project URL | `WithProjectURL` | - | `SUPABASE_URL` | none, **required** |
| Service key | `WithServiceKey` | - | `SUPABASE_SERVICE_ROLE_KEY` | none, **required** |
| Schema | `WithSchema` | `?schema=` | `SUPABASE_VAULT_SCHEMA` | `public` |
| Relation | `WithView` | `?view=` | `SUPABASE_VAULT_VIEW` | `decrypted_secrets` |

The environment is read lazily at the first resolve, so registering this provider
from a blank import is safe even when no credentials exist at process start.

### Options

| Option | Effect |
| --- | --- |
| `WithProjectURL(u)` | Set the `https://<project-ref>.supabase.co` origin; `/rest/v1` is appended for you and a trailing slash is trimmed |
| `WithServiceKey(key)` | Set the service-role key, captured in a closure rather than a field; an empty key falls back to `SUPABASE_SERVICE_ROLE_KEY` |
| `WithSchema(name)` | Set the exposed schema for refs with no `?schema=` |
| `WithView(name)` | Set the relation name for refs with no `?view=`; a name containing a slash is rejected, since it is one PostgREST resource rather than a path |
| `WithAllowInsecure()` | Permit an `http://` project URL, and nothing else |
| `WithHTTPClient(c)` | Inject a custom `*http.Client`; a nil client is a no-op |

`Close()` is idempotent and terminal: after it returns, every `Resolve`
reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting
Supabase. Without `WithHTTPClient` it releases nothing: this provider builds
no default client of its own to hold onto. A client injected with
`WithHTTPClient` is never closed: `Close` may return its idle connections to
the pool, but leaves the client usable.

### Local development

`supabase start` serves the Data API from `http://127.0.0.1:54321`, so `http://`
has to be reachable somehow. It requires `WithAllowInsecure()`, which takes no
argument deliberately: every request carries a key that bypasses row-level
security on the whole project, so an opt-in that cannot be switched on by a
`bool` variable that happened to default to `true` is the safer shape. It permits
cleartext `http` and nothing else, never a way to skip the scheme check itself
(`TestAllowInsecurePermitsHTTPAndNothingElse`).

The scheme is checked against a closed set of `https` and `http`, because
`httpcore.New` requires only a scheme and a host: an `ftp://` typo would
otherwise construct cleanly and then fail on every resolve with `net/http`'s
"unsupported protocol scheme".

Both checks run on the first `Resolve` rather than in `New`, because `New` returns
no error: keeping it single-valued is what lets a blank import register the scheme
from `init`. `mamori doctor` resolves every ref before deployment, so a
misconfiguration still surfaces before production.

## Testing status

Wire shapes are pinned from vendor documentation on 2026-08-04. **Nobody on this
project has a Supabase project**, so the rows below marked **Documented, not
live-verified** are exactly that: taken from the vendor's reference rather than
confirmed against a live call. They are not claimed to be more than that, and the
integration test below is how the first person with a project closes the gap.

Sources:

- Supabase Vault, and the `vault.decrypted_secrets` columns:
  <https://supabase.com/docs/guides/database/vault>
- PostgREST schema selection via `Accept-Profile`, and `db-schemas`:
  <https://postgrest.org/en/stable/references/api/schemas.html>
- Supabase custom schemas, "Exposed schemas", and the `Accept-Profile` curl form:
  <https://supabase.com/docs/guides/api/using-custom-schemas>
- Restriction of the `auth`, `storage`, `realtime` and `vault` schemas:
  <https://github.com/orgs/supabase/discussions/34270>
- PostgREST horizontal filtering (`?col=eq.value`), vertical filtering
  (`?select=`), and the array response:
  <https://docs.postgrest.org/en/v12/references/api/tables_views.html>
- PostgREST status codes and the `message`/`hint`/`details`/`code` envelope:
  <https://postgrest.org/en/stable/references/errors.html>
- The empty-array-vs-406 distinction for zero rows:
  <https://docs.postgrest.org/en/v12/references/api/resource_representation.html>
- Supabase API keys, `BYPASSRLS`, and the "never expose secret keys" guidance:
  <https://supabase.com/docs/guides/api/api-keys>

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-process fake `http.RoundTripper` (`go test ./...`) |
| That `vault` cannot be added to Exposed schemas, so project-side setup is unavoidable | **Documented, not live-verified** ([discussion #34270](https://github.com/orgs/supabase/discussions/34270)); `TestIntegrationVaultSchemaIsNotReachable` is how a project owner confirms it |
| Request shape (`/rest/v1/<view>?name=eq.<n>&select=...`, `Accept-Profile`, both credential headers) | **Documented, not live-verified**; what this provider sends is pinned by `TestResolveSendsTheDocumentedPostgRESTQuery`, `TestResolveSendsBothCredentialHeaders`, `TestResolveAlwaysSendsAcceptProfile` |
| Response shape: a JSON array even for one row | **Documented, not live-verified** ([tables and views](https://docs.postgrest.org/en/v12/references/api/tables_views.html)) |
| **An absent row is `200` with `[]`, never `404`** | **Documented, not live-verified** ([resource representation](https://docs.postgrest.org/en/v12/references/api/resource_representation.html)); `TestIntegrationAbsentSecretIsNotFound` is the live check |
| The empty array maps to `ErrNotFound` and nothing else, so a field's `default:` applies | **Verified** (`TestEmptyArrayIsNotFoundAndNothingElse`, `TestEmptyArrayAppliesTheFieldDefault`, plus `providertest`'s `NotFoundTyped`) |
| A misconfiguration is never reported as `not_found` | **Verified** (`TestNotFoundIsNotReportedForAMisconfiguration`, six cases) |
| Value mapping: `decrypted_secret` to `Bytes`, `updated_at` to `Version`, `Sensitive` always true | **Verified** (`TestResolveReturnsTheDecryptedSecret`, `TestResolveUsesUpdatedAtAsVersion`) |
| `Version` changes on a rewrite of identical bytes, which a content hash could not report | **Verified** (`TestResolveVersionChangesWhenOnlyTheTimestampDoes`) |
| Content-hash `Version` fallback, and one version per secret rather than per selected fragment | **Verified** (`TestResolveHashesWhenTheRelationOmitsUpdatedAt`, `TestResolveVersionIgnoresKeySelection`) |
| Scope precedence: ref option over provider option over environment over default | **Verified** (`TestSettingsPrecedence`, `TestRefOptionsReachTheWire`) |
| Credential precedence: explicit option over environment variable, read lazily at resolve time | **Verified** (`TestCredentialsReadFromEnvironment`, `TestExplicitCredentialsBeatTheEnvironment`) |
| A secret name with slashes, dots or reserved characters is one literal name, never a traversal | **Verified** (`TestSecretNameIsTheWholePath`, `TestFilterValueQuotesOnlyWhenNeeded`) |
| A secret whose value is the empty string resolves, rather than looking absent | **Verified** (`TestResolveEmptySecretIsNotNotFound`) |
| `#field` and `#/json/pointer` selection through `mamori.SelectKey` | **Verified** (`TestResolveVersionIgnoresKeySelection`, `TestResolveSelectsWithPointer`, plus `providertest`'s `JSONPointerSelection`) |
| Every status PostgREST documents (400/401/402/403/404/405/406/415/416/500/503/504, plus 429 and 502 from a proxy) maps to its kind | **Verified** (`TestStatusToKind`, plus `providertest`'s `ErrorClassification`) |
| Only `message` and `code` are surfaced; `details` and `hint` never are | **Verified** (`TestErrorDetailShape`, `TestErrorDetailNeverReadsDetailsOrHint`, `TestErrorDetailSurfacesTheVendorMessageAndCode`) |
| No service-role key and no secret value reaches an error, *including when the backend echoes them in its error envelope* | **Verified** (`TestResolveErrorCarriesNoServiceKey`, `TestResolveErrorCarriesNoSecretValue`, `TestResolveMalformedResponseNeverEchoesTheBody`) |
| The key never appears in `%v`, `%+v` or `%#v` of the `Provider`, before or after a resolve | **Verified** (`TestProviderNeverPrintsTheServiceKey`) |
| The value is never cached; every `Resolve` is a live read | **Verified** (`TestResolveIsNotCached`, `TestDeletedSecretBecomesNotFound`) |
| Project URL scheme is a closed set; `http://` needs `WithAllowInsecure()`, which rescues nothing else | **Verified** (`TestResolveRejectsANonHTTPProjectURL`, `TestResolveRejectsAnInsecureProjectURL`, `TestAllowInsecurePermitsHTTPAndNothingElse`) |
| `WatchableProvider` is deliberately not implemented | **Verified** (`TestProviderIsNotWatchable`) |
| End to end against a real Supabase project: that the documented wire shapes are what the backend actually sends | **Needs a live backend** - see the integration test below |

The unit and conformance tests run against an in-process fake `http.RoundTripper`
that emulates the PostgREST endpoint, including an absent row as `200` with `[]`
and injectable failures whose error envelopes deliberately echo the credential
and the secret value. `go test ./...` therefore requires **no** network access and
**no** Supabase project.

### Live integration test

Guarded by a build tag; skips unless `SUPABASE_URL`,
`SUPABASE_SERVICE_ROLE_KEY` and `SUPABASE_TEST_SECRET_NAME` are all set. It never
logs the service-role key or a resolved value: only the secret name, a byte
count, and a version.

```sh
export SUPABASE_URL=https://<project-ref>.supabase.co
export SUPABASE_SERVICE_ROLE_KEY=...
export SUPABASE_TEST_SECRET_NAME=some-existing-vault-secret
export SUPABASE_VAULT_SCHEMA=public          # optional, defaults to public
export SUPABASE_VAULT_VIEW=decrypted_secrets # optional
GOWORK=off go test -tags integration -run Integration ./...
```

`TestIntegrationVaultSchemaIsNotReachable` is the one to watch: it asserts that
reading `vault.decrypted_secrets` **directly** still fails. If it ever starts
passing, Supabase has relaxed the restriction, and this provider's whole setup
requirement can be dropped.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/supabase
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go vet -tags integration ./...
GOWORK=off go test ./...
GOWORK=off go test -race ./...
```
