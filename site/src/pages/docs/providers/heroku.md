---
layout: ../../../layouts/DocsLayout.astro
title: Heroku Config Vars
---

# Heroku Config Vars

Load a value from [Heroku config vars](https://devcenter.heroku.com/articles/config-vars), the environment-variable store the Heroku platform injects into every dyno of an app. Pure `net/http` on top of [`httpcore`](/docs/writing-a-provider/httpcore/), no third-party SDK.

| | |
| --- | --- |
| Scheme | `heroku://` |
| Module | `github.com/xavidop/mamori/providers/heroku` |
| Sensitive | **yes**, always |
| Watch | poll |
| Batch | **yes** - one request per app, not one per ref |
| Auth | `HEROKU_API_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/heroku
```

```go
import _ "github.com/xavidop/mamori/providers/heroku"
```

## Using the ref

```
heroku://<VAR>[#<key>]              the app comes from configuration
heroku://<app>/<VAR>[#<key>]        the app is named in the ref
```

`<app>` is Heroku's `{app_id_or_name}`: either an app name (`my-app`) or its UUID. `<VAR>` is the config var name. `#<key>` selects into the value when that value is itself JSON, either as an RFC 6901 JSON Pointer (`#/db/password`) or as a literal top-level key (`#password`).

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

A third path segment is refused with `mamori.ErrInvalid` rather than having one segment silently ignored.

### Why the app is a path segment

[`cloudflare-kv://`](/docs/providers/cloudflare-kv/) selects its namespace with a `?namespace=` query option, and this provider follows its *precedence* rule exactly while saying it differently. cloudflare-kv cannot use a path segment because a Workers KV key may itself contain slashes, so splitting a path would misread one such key as two segments.

A Heroku config var name cannot. Heroku's published JSON schema constrains a config var key to `^\w+$`, and the Dev Center tells operators to use "only alphanumeric characters and the underscore character (`_`)". A slash in a `heroku://` ref is therefore unambiguous.

That pattern is not enforced client-side: re-implementing a vendor's validation could only ever reject a name the backend might have accepted.

### App precedence

| Rank | Source |
| --- | --- |
| 1 | the ref path, `heroku://<app>/<VAR>` |
| 2 | `heroku.WithApp("my-app")` |
| 3 | `HEROKU_APP` (the variable the Heroku CLI reads for its `-a` flag) |
| 4 | `HEROKU_APP_NAME` |

`HEROKU_APP_NAME` is last on purpose. Heroku injects it into every dyno once `heroku labs:enable runtime-dyno-metadata` is on, so an app running on Heroku that reads its own config vars needs no configuration at all - but nobody *chose* it, and an operator who named a different app meant the one they named.

A ref that ends up with no app fails with `mamori.ErrInvalid`, never `not_found`: `not_found` is the one kind that makes mamori apply a field's `default:`, so a forgotten `HEROKU_APP` must not become a silently defaulted value.

## Authentication

```bash
heroku authorizations:create -d "mamori"
export HEROKU_API_KEY=HRKU-...
export HEROKU_APP=my-app
```

`HEROKU_API_KEY` is the same variable the Heroku CLI reads, so a machine already set up to run `heroku config` needs nothing further. Configure explicitly when you prefer:

```go
mamori.Load[Config](ctx, mamori.WithProvider(
    heroku.New(heroku.WithAPIKey(token), heroku.WithApp("my-app")),
))
```

The token is held in a closure rather than a struct field, because `fmt`'s `%+v` and `%#v` reach unexported fields by reflection and no redaction method can stop them.

### The `Accept` version header

Every request carries `Accept: application/vnd.heroku+json; version=3`. The Platform API reference is explicit that this is required, and answers **406 `not_acceptable`** without it - with a message that names neither the app nor the token, which is why it is the failure a hand-rolled Heroku client hits first and understands last. The provider always sends it; there is no option to change the version, because a version bump changes the response shape rather than only the URL.

## Batching

This is a `mamori.BatchProvider`. One `GET` returns **every** config var of an app in one document, so a config with twelve `heroku://` fields costs **one** request per app rather than twelve. mamori uses it automatically on the `Load` path.

The saving is not cosmetic: Heroku meters an account at 4500 requests per hour and answers `429 rate_limit` past that, so twelve fields polled every 30 seconds is 1440 requests an hour unbatched and 120 batched.

Omitted from the batch rather than failing it, so each field's `default:` applies:

- a config var absent from its app's document;
- a config var whose value is JSON `null`;
- every ref of an app that answers 404, since on this endpoint a 404 can only mean the app is absent or invisible to the token.

A `401`, `403`, `429` or `5xx` still fails the batch. Rendering an expired token as "every field took its default" is the quietest possible way to deploy a broken configuration.

## Sensitive is always true

Every value this provider returns is marked sensitive, with no per-ref switch, even though config vars are nominally "application configuration":

- The endpoint returns the app's whole config var namespace with **no per-var classification**. Nothing in the response separates `LOG_LEVEL` from `DATABASE_URL`.
- Heroku add-ons write live credentials into that namespace **without the operator typing them**: Heroku Postgres creates `DATABASE_URL`, password and all; Redis creates `REDIS_URL`.
- The two mistakes are not symmetric. Marking a log level sensitive costs a redacted debug line; marking a database password non-sensitive puts it in a log.

`mamori vet` therefore flags a `heroku://` ref stored in a plain `string`. Use [`secret.String`](/docs/concepts/secret-types/).

## Change detection

`Value.Version` is a content hash of the whole config var value - not of the selected `#field`, and **not the response `ETag`**.

The endpoint does send an `ETag`, and `httpcore.Version` prefers one, so the obvious code is wrong here: an `ETag` covers the whole document, so editing any one var would change the `Version` of every ref pointing at that app, producing a spurious `PreApply` and `OnChange` for every field but the one that moved.

`httpcore.Revalidator` is deliberately not used either. A `304` still costs a request token - Heroku's limit is on requests, not bytes - so a conditional GET would save bandwidth rather than quota, while batching already removes the cost that matters.

## Watching

The Platform API exposes no streaming or blocking read of config vars, so this provider does not implement `mamori.WatchableProvider`. mamori polls it and uses `Value.Version` to detect a change between ticks.

## Error classification

| HTTP | Heroku `id` | mamori kind |
| --- | --- | --- |
| 400 | `bad_request` | `invalid` |
| 401 | `unauthorized` | `unauthenticated` |
| 402 | `delinquent` | `unavailable` |
| 403 | `forbidden`, `suspended` | `permission_denied` |
| 404 | `not_found` | `not_found` |
| 406 | `not_acceptable` | `unavailable` |
| 422 | `invalid_params` | `invalid` |
| 429 | `rate_limit` | `rate_limited` |
| 500, 503 | | `unavailable` |

Classification is `httpcore.ClassifyStatus`, unmodified. 402, 406 and 410 are terminal in practice but land in that table's transient default; overriding it per provider is exactly the drift `httpcore` exists to prevent, and in practice 402 heals without a code change while 406 is unreachable while version 3 exists.

**A mistyped app name presents as every ref against it falling back to its default**, because a 404 here means the app is absent, and mamori's `not_found` is what makes a `default:` apply. That is worth knowing, because it is the kind of thing a misconfiguration hides behind rather than announces.

Only Heroku's `id` field ever reaches an error message, never `message` and never the body: `id` comes from a closed, documented vocabulary and so cannot carry a value, while on the success path this endpoint's body is the app's entire config var document.

## Reference

- [Platform API: Config Vars](https://devcenter.heroku.com/articles/platform-api-reference#config-vars)
- [Platform API: clients, versioning and error responses](https://devcenter.heroku.com/articles/platform-api-reference)
- [Configuration and Config Vars](https://devcenter.heroku.com/articles/config-vars)
- [Dyno metadata](https://devcenter.heroku.com/articles/dyno-metadata)
- [Published JSON schema](https://github.com/heroku/platform-api/blob/main/schema.json)

The module README records which rows of that contract are live-verified and which are documented only: nobody on this project has a Heroku account, so the wire shape comes from the vendor reference and the `//go:build integration` tests are how the first person with a token confirms it.
