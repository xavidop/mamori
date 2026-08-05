# mamori - Heroku config vars provider

Resolves `heroku://` refs against [Heroku config vars](https://devcenter.heroku.com/articles/config-vars),
the environment-variable store the Heroku platform injects into every dyno of an
app.

One `GET` returns **every** config var of an app in one document, which is the
fact that shapes this whole module: it is a `mamori.BatchProvider`, so a config
with twelve `heroku://` fields costs **one** request, not twelve.

## Install

```sh
go get github.com/xavidop/mamori/providers/heroku
```

```go
import _ "github.com/xavidop/mamori/providers/heroku"
```

A blank import is enough. The provider registers itself and reads
`HEROKU_API_KEY`, `HEROKU_APP` and `HEROKU_APP_NAME` lazily at resolve time, so
registering from `init` is safe even when no credentials exist at process start.

## Ref grammar

```
heroku://<VAR>[#<key>]              the app comes from configuration
heroku://<app>/<VAR>[#<key>]        the app is named in the ref
```

`<app>` is Heroku's `{app_id_or_name}`: either an app name (`my-app`) or its
UUID. `<VAR>` is the config var name. `#<key>` selects into the value when that
value is itself JSON, through `mamori.SelectKey`, so it is either an RFC 6901
JSON Pointer (`#/db/password`) or a literal top-level key (`#password`) exactly
as in every other provider.

```go
type Config struct {
    DatabaseURL secret.String `source:"heroku://DATABASE_URL"`
    Port        int           `source:"heroku://PORT" default:"8080"`
    // A different app, named in the ref.
    WorkerToken secret.String `source:"heroku://my-worker/API_TOKEN"`
    // A config var whose value is a JSON blob.
    SigningKey  secret.String `source:"heroku://SERVICE_ACCOUNT#/private_key"`
}
```

**A third path segment is refused** with `mamori.ErrInvalid` rather than having
one segment silently ignored. A ref that quietly means something other than what
it says is worse than one that fails, and `mamori doctor` resolves every ref
before deployment.

### Why the app is a path segment and not `?app=`

`providers/cloudflare-kv` selects its namespace with a `?namespace=` query
option, and this module follows its **precedence** rule exactly - the ref beats
the provider option, which beats the environment - while saying it differently.
The reason cloudflare-kv cannot use a path segment is that a Workers KV key may
itself contain slashes, so splitting a path would misread one such key as two
segments.

A Heroku config var name cannot. The vendor's own published JSON schema
constrains a config var key to the pattern `^\w+$`, and the Dev Center tells
operators to use "only alphanumeric characters and the underscore character
(`_`)". A slash in a `heroku://` ref path is therefore unambiguous, and
`heroku://my-app/DATABASE_URL` reads better than a query string.

That schema pattern is **not enforced here**. It is a vendor-side rule, and
re-implementing it client-side could only ever reject a name the backend might
have accepted, turning "the backend would have told you" into "mamori refused
before asking".

### App precedence

| Rank | Source | Notes |
| --- | --- | --- |
| 1 | the ref path, `heroku://<app>/<VAR>` | one provider can serve refs spanning several apps |
| 2 | `heroku.WithApp("my-app")` | |
| 3 | `HEROKU_APP` | the same variable the Heroku CLI reads for its `-a` flag |
| 4 | `HEROKU_APP_NAME` | injected by the platform itself, see below |

`HEROKU_APP_NAME` is last on purpose. Heroku sets it in every dyno once
`heroku labs:enable runtime-dyno-metadata` is on, so **an app running on Heroku
that reads its own config vars needs no configuration at all** - but nobody
*chose* it, and an operator who named a different app meant the one they named.

A ref that ends up with no app at all fails with `mamori.ErrInvalid`, never
`ErrNotFound`. That distinction is load bearing: `ErrNotFound` is the one kind
that makes mamori apply a field's `default:`, so reporting a forgotten
`HEROKU_APP` as one would turn a misconfiguration into a silently defaulted
value.

## Authentication

A Heroku API token in the `Authorization` header, from `WithAPIKey` or from
`HEROKU_API_KEY` - the same variable the Heroku CLI reads, so a machine already
set up to run `heroku config` needs nothing further.

```sh
heroku authorizations:create -d "mamori"
export HEROKU_API_KEY=HRKU-...
```

```go
mamori.Load[Config](ctx, mamori.WithProvider(
    heroku.New(heroku.WithAPIKey(token), heroku.WithApp("my-app")),
))
```

The token is held in a **closure, never a struct field**. `fmt`'s `%+v` and
`%#v` walk unexported fields by reflection, and reflection cannot call a
`String` method on a value it reaches that way, so no redaction method would
stop a debug dump or a panic trace printing a plain field in cleartext. A
`Provider` is exactly the kind of value that gets printed: it is what an
application hands to `mamori.WithProvider`.

### The `Accept` version header

Every request carries:

```
Accept: application/vnd.heroku+json; version=3
```

The reference is explicit that this is **required**: *"Clients must address
requests to `api.heroku.com` using HTTPS and specify the
`Accept: application/vnd.heroku+json; version=3` Accept header."* A request
without it is answered **406 `not_acceptable`** with the message *"request
failed, set `Accept: application/vnd.heroku+json; version=3` header and try
again"* - a message that names neither the app nor the token, which is why this
is the failure a hand-rolled Heroku client hits first and understands last.

There is no `WithAPIVersion` option. A version bump changes the response shape,
not only the URL, so an option to ask for a version this code cannot parse would
trade one legible failure for an illegible one.

The package's own fake backend **enforces** the header (it answers 406 without
it) rather than merely recording it, so
`TestResolveSendsTheRequiredAcceptHeader` is a test a provider that dropped the
header could not pass.

## Options

| Option | Default | Purpose |
| --- | --- | --- |
| `WithAPIKey(token)` | `HEROKU_API_KEY` | the Platform API token |
| `WithApp(app)` | `HEROKU_APP`, then `HEROKU_APP_NAME` | default app for refs that name none |
| `WithBaseURL(u)` | `https://api.heroku.com` | a proxy or a test double |
| `WithAllowInsecure()` | off | permit an `http://` base URL, and nothing else |
| `WithHTTPClient(c)` | 30s timeout | a proxy, a custom transport, an in-process fake |

An option given an empty string is ignored rather than pinning an unusable empty
value, so `WithAPIKey(os.Getenv("SOMETHING_ELSE"))` on an unset variable still
falls back to `HEROKU_API_KEY`.

`WithAllowInsecure` takes no argument deliberately. The token travels in an
`Authorization` header on every request, and that one token reaches every config
var of every app the account can see; an opt-in that cannot be switched on by a
`bool` variable that happened to default to `true` is the safer shape. A scheme
that is neither `http` nor `https` is refused either way, because
`httpcore.New` requires only a scheme and a host, so an `ftp://` typo would
otherwise construct cleanly and fail on every single resolve with net/http's
"unsupported protocol scheme".

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports
`errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Heroku.
Without `WithHTTPClient` it releases nothing: this provider builds no default
client of its own to hold onto. A client injected with `WithHTTPClient` is
never closed or invalidated, only its idle connections are returned to the
pool, and only when that client's `Transport` is non-nil (a nil `Transport`
resolves to the shared `http.DefaultTransport`, which `Close` leaves alone).

## Batching

`ResolveBatch` groups refs by app and issues **one request per app**. mamori
calls it automatically on the `Load` path.

The saving is not cosmetic. The Platform API has no single-config-var endpoint:
reading one var and reading all of them are the same request. Heroku meters an
account at **4500 requests per hour** and answers `429 rate_limit` past that, so
twelve fields polled every 30 seconds is 1440 requests an hour through `Resolve`
and 120 through `ResolveBatch`.

Three things are omitted from the result map rather than failing the batch, so
mamori applies each field's `default:`:

- a config var absent from its app's document;
- a config var whose value is JSON `null` (see below);
- **every ref of an app that answers 404**, since on this endpoint a 404 can
  only mean the app is absent or invisible to the token. One mistyped app name
  among many refs must not take the whole batch down with it.

Everything else still fails the batch: a `401`, `403`, `429` or `5xx` is a real
failure, and an `ErrInvalid` from selecting a `#field` of a value that is not a
JSON object is a malformed request against that payload rather than an absence.
Rendering an expired token as "every field took its default" is the quietest
possible way to deploy a broken configuration.

`Resolve` and `ResolveBatch` agree on the full `mamori.Value` - `Bytes`,
`Version`, `Sensitive` and `Metadata` - and a test asserts that rather than
comparing `Bytes` alone.

## `Value.Sensitive` is always true

**Every value this provider returns is marked sensitive, with no per-ref or
per-provider switch.** Heroku config vars are nominally "application
configuration", so this deserves an argument rather than an assertion:

- The endpoint hands back the app's whole config var namespace in one document
  with **no per-var classification**. There is no field, flag, or convention in
  the response that separates `LOG_LEVEL` from `DATABASE_URL`.
- Heroku add-ons write live credentials into that same namespace **without the
  operator typing them**: provisioning Heroku Postgres creates `DATABASE_URL`,
  password and all; Redis creates `REDIS_URL`. A namespace that fills itself
  with credentials cannot be treated as configuration by default.
- Heroku's own guidance for the store is that it "prevents secure credentials
  from being stored in version control", and warns operators to "avoid using
  direct references to sensitive environment variables where your app code
  writes to standard out".
- The two mistakes are not symmetric. Marking a log level sensitive costs a
  redacted debug line. Marking a database password non-sensitive puts it in a
  log.

`mamori vet` therefore flags a `heroku://` ref stored in a plain `string`, and
`cmd/mamori`'s scheme list includes `heroku` for the same reason.

## Change detection

`Value.Version` is a content hash of the **whole config var value**, not of the
selected `#field` and **not the response `ETag`**.

The endpoint does send an `ETag`, and `httpcore.Version` prefers one over a body
hash, so `Version: httpcore.Version(resp, body)` is the obvious code and is
wrong. An `ETag` describes the whole document, so editing any one var would
change the `Version` of every ref pointing at that app. mamori compares
`Version` instead of bytes, so every field would report changed: a spurious
`PreApply` and `OnChange` each, and for a rotating credential a spurious
reconnect.

Hashing the whole var value rather than the selected fragment is the other half:
two refs selecting different `#fields` of one var must agree on when that var
changed.

### Why there is no conditional GET

`httpcore.Revalidator` turns a repeated poll into a conditional `GET`, and this
endpoint supplies both an `ETag` and a `Last-Modified`. It is deliberately not
used:

- A `304` still costs a request token. Heroku's limit is on **requests**, not
  bytes, so a conditional GET saves bandwidth and not quota, while
  `ResolveBatch` already removes the actual cost, the per-field fan-out.
- The cached body would be handed back whole anyway, so per-ref change detection
  would still come from the same per-var hash. The revalidator would change
  nothing a caller can observe.
- It adds cross-call state whose invalidation is one more thing to get wrong,
  for a saving that is not the one that matters here.

## Watching

The Platform API exposes no streaming or blocking read of config vars, so this
provider deliberately does **not** implement `mamori.WatchableProvider`. mamori
wraps it in the polling adapter and uses `Value.Version` to detect a change
between ticks. A test asserts the type does not satisfy the interface, so the
conformance kit's watch cases skip because there is genuinely no `Watch`, not
because a flag told them to.

## `null` config vars

Heroku's published schema types a config var value as `["string", "null"]`, not
`"string"`: `null` is how the `PATCH` form of this endpoint deletes a var. This
module decodes the document into `map[string]*string` and treats a `null`
exactly like an absent name - `mamori.ErrNotFound`, so the field's `default:`
applies.

Decoding into `map[string]string` instead compiles, passes almost every test,
and resolves a `null` var to the **empty string**, silently applying `""` where
the default belonged. A config var whose value is legitimately `""` still
resolves; that is what the pointer keeps apart.

## Error classification

Classification comes from `httpcore.ClassifyStatus`, unmodified. Every status
the reference documents:

| HTTP | Heroku `id` | mamori kind |
| --- | --- | --- |
| 400 | `bad_request` | `invalid` |
| 401 | `unauthorized` | `unauthenticated` |
| 402 | `delinquent` | `unavailable` |
| 403 | `forbidden`, `suspended` | `permission_denied` |
| 404 | `not_found` | `not_found` |
| 406 | `not_acceptable` | `unavailable` |
| 409 | `conflict` | `unavailable` |
| 410 | `gone` | `unavailable` |
| 416 | `requested_range_not_satisfiable` | `unavailable` |
| 422 | `invalid_params`, `verification_needed` | `invalid` |
| 429 | `rate_limit` | `rate_limited` |
| 500, 503 | | `unavailable` |

Three rows are honest rather than ideal. **402, 406 and 410 are terminal in
practice** but land in `httpcore`'s transient default, so mamori will back off
and retry them. That is accepted rather than overridden: `httpcore` exists so
one table classifies every HTTP-backed provider, a provider-local override is
exactly the drift its README warns about, and it would desynchronise this module
from `httpcore.StatusForKind`, the inverse the conformance kit's `Fail` hook
depends on. In practice 402 does heal without a code change (someone pays the
bill), and 406 is unreachable while version 3 exists, since this provider always
sends the header.

**404 is the load-bearing row**, because it is the only kind that changes what
mamori *does* rather than what it reports. On this endpoint it means the app is
absent or invisible to the token, never that one config var is missing - an
absent name is simply absent from a successful 200 document. Be aware of the
consequence, because it is the kind of thing a misconfiguration hides behind
rather than announces: **a mistyped app name presents as every ref against it
falling back to its default**, exactly as a genuinely absent config var would.

### Nothing from a response body reaches an error

`httpcore` leaves its `ErrorDetail` hook nil by default because a response body
can be the resolved value. This module supplies one that lifts **only** the
`id` field, and that choice is deliberate over the more obvious `message`:

- `id` is a **closed, documented vocabulary** (`bad_request`, `unauthorized`,
  `delinquent`, ... `rate_limit`), so it is a value from a fixed list and can
  never carry anything the caller sent or anything the app stores.
- `message` is free prose the vendor may reword at any time, and on the success
  path this endpoint's body is the app's **entire config var document**: every
  credential it holds. "The field that cannot contain a value" is a stronger
  guarantee than "the field that currently does not".

A JSON decode failure drops its cause rather than wrapping it with a second
`%w`: `encoding/json` quotes the offending byte in a syntax error, and on the
success path that byte is part of a config var. The test for this asserts the
absence of encoding/json's own phrasing, not just of a body substring, because
the leak is a single character and no substring long enough to be meaningful
would ever match it.

## Testing status

Nobody on this project has a Heroku account, so it is worth being precise about
which rows below are **live-verified** and which are **documented**. The wire
shape comes from the vendor's API reference and its published JSON schema,
pinned on 2026-08-04, not from a live call.

| Behaviour | How it is verified |
| --- | --- |
| Ref grammar, app precedence, batching, classification, hygiene | **live-verified** against an in-process fake, `go test -race` |
| Conformance kit (`providertest.Run`), goroutine leaks, context cancellation | **live-verified** |
| Path template `GET /apps/{app_id_or_name}/config-vars` | **documented** ([reference](https://devcenter.heroku.com/articles/platform-api-reference#config-vars), [schema](https://github.com/heroku/platform-api/blob/main/schema.json)) |
| Required `Accept: application/vnd.heroku+json; version=3`, 406 without it | **documented** ([reference](https://devcenter.heroku.com/articles/platform-api-reference#clients)) |
| `Authorization: Bearer {HEROKU_TOKEN}` | **documented** ([quickstart](https://devcenter.heroku.com/articles/platform-api-quickstart)) |
| Flat `{"FOO":"bar"}` response with no envelope | **documented** ([reference](https://devcenter.heroku.com/articles/platform-api-reference#config-vars)) |
| Value type `["string","null"]` | **documented** ([schema](https://github.com/heroku/platform-api/blob/main/schema.json), `config-var.definitions.config_vars`) |
| Status codes and error `id` vocabulary | **documented** ([reference](https://devcenter.heroku.com/articles/platform-api-reference#error-responses)) |
| 4500 requests/hour, `RateLimit-Remaining` | **documented** ([reference](https://devcenter.heroku.com/articles/platform-api-reference#rate-limits)) |
| Key pattern `^\w+$`, 64 kB total config var data | **documented** ([config vars](https://devcenter.heroku.com/articles/config-vars), [schema](https://github.com/heroku/platform-api/blob/main/schema.json)) |
| `HEROKU_APP_NAME` from dyno metadata | **documented** ([dyno metadata](https://devcenter.heroku.com/articles/dyno-metadata)) |

One ambiguity is worth recording rather than hiding: the schema declares the
value type **twice**, as `["string"]` on the `config-var` definition itself and
as `["string","null"]` on the nested `config_vars` definition. The `GET`
endpoint's `targetSchema` points at the nested one, so `["string","null"]` is
what this module implements.

The `//go:build integration` tests are how the first person with an API token
confirms all of it in one command:

```sh
export HEROKU_API_KEY=$(heroku auth:token)
export HEROKU_APP=my-app
export HEROKU_TEST_CONFIG_VAR=SOME_EXISTING_VAR
cd providers/heroku && GOWORK=off go test -tags integration -run Integration ./...
```

They skip cleanly when any of the three is unset, and log only a byte count, a
version, and the config var *name* - never a token and never a value. One of
them deliberately bypasses the provider to issue the same `GET` **without** the
version header, so a future Heroku change making it optional surfaces as a test
failure rather than as a mystery.

## Development

This package is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/heroku
GOWORK=off go mod tidy
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go vet -tags integration ./...
```
