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
| Sensitive | yes, always |
| Watch | poll |
| Auth | `HEROKU_API_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/heroku
```

```go
import _ "github.com/xavidop/mamori/providers/heroku"
```

## Using the ref

A `heroku://` ref points at one config var of one app, optionally selecting a field from a JSON value stored there.

```text
heroku://<VAR>[#field-or-pointer]
heroku://<app>/<VAR>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<app>` | no | Heroku's `{app_id_or_name}`: an app name such as `my-app`, or its UUID. Left out, the app comes from configuration. |
| `<VAR>` | yes | The config var name, e.g. `DATABASE_URL`. |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued config var via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `heroku://DATABASE_URL` reads `DATABASE_URL` from the configured app.
- `heroku://my-worker/API_TOKEN` reads `API_TOKEN` from the app `my-worker`.
- `heroku://SERVICE_ACCOUNT#/private_key` selects `private_key` from a config var whose value is a JSON document.

```go
type Config struct {
	DatabaseURL secret.String `source:"heroku://DATABASE_URL"`
	Port        int           `source:"heroku://PORT" default:"8080"`
	WorkerToken secret.String `source:"heroku://my-worker/API_TOKEN"`
	SigningKey  secret.String `source:"heroku://SERVICE_ACCOUNT#/private_key"`
}
```

A third path segment is refused with `mamori.ErrInvalid` rather than having one segment silently ignored.

Every value is `Sensitive`, with no per-ref switch. The API hands back an app's whole config var namespace with nothing separating `LOG_LEVEL` from `DATABASE_URL`, and Heroku add-ons write live credentials into that namespace without the operator typing them: provisioning Heroku Postgres creates `DATABASE_URL`, password and all. `mamori vet` therefore flags a `heroku://` ref stored in a plain `string` - use [`secret.String`](/docs/concepts/secret-types/).

### Which app a ref reads

A ref that names no app takes one from configuration. First match wins:

| Rank | Source |
| --- | --- |
| 1 | the ref path, `heroku://<app>/<VAR>` |
| 2 | `heroku.WithApp("my-app")` |
| 3 | `HEROKU_APP`, the variable the Heroku CLI reads for its `-a` flag |
| 4 | `HEROKU_APP_NAME`, injected into every dyno once `heroku labs:enable runtime-dyno-metadata` is on |

`HEROKU_APP_NAME` is last, so an app running on Heroku that reads its own config vars needs no configuration at all, while an operator who named a different app still gets the one they named. A ref that ends up with no app fails with `mamori.ErrInvalid`, never `not_found`, so a forgotten `HEROKU_APP` is a loud error rather than a silently defaulted value.

## One request per app

One `GET` returns **every** config var of an app, so this provider is a `mamori.BatchProvider`: a config with twelve `heroku://` fields costs **one** request per app rather than twelve, and `mamori.Load` uses that path automatically. The saving is quota, not cosmetics. Heroku meters an account at 4500 requests per hour and answers `429` past that, so twelve fields polled every 30 seconds is 1440 requests an hour unbatched and 120 batched.

Three things are omitted from a batch rather than failing it, so each field's `default:` applies: a config var absent from its app's document, a config var whose value is JSON `null` (which is how Heroku deletes one), and every ref of an app that answers 404. A `401`, `403`, `429` or `5xx` still fails the batch, because rendering an expired token as "every field took its default" is the quietest possible way to deploy a broken configuration.

## Error classification

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

**A mistyped app name presents as every ref against it falling back to its `default:`.** A 404 on this endpoint can only mean the app is absent or invisible to your token, and `not_found` is the one kind that makes a `default:` apply. If a whole app's worth of fields defaults at once, check the app name before you check the vars.

Only Heroku's `id` field ever reaches an error message, never `message` and never the response body: on the success path that body is the app's entire config var document.

## Watch

The Platform API exposes no streaming or blocking read of config vars, so this provider does not implement `WatchableProvider`; mamori polls it (`WithPollInterval` + jitter) and compares `Value.Version` between ticks. That version is a content hash of the config var's own value rather than the response `ETag`, so editing one var of an app does not fire `OnChange` for every other ref pointing at it. Compose [`middleware.Cache`](/docs/middleware/) in front of it to coalesce reads across a poll interval.

## Configuration

```bash
heroku authorizations:create -d "mamori"
export HEROKU_API_KEY=HRKU-...
export HEROKU_APP=my-app
```

`HEROKU_API_KEY` is the same variable the Heroku CLI reads, so a machine already set up to run `heroku config` needs nothing further. Configure explicitly when you prefer:

```go
import "github.com/xavidop/mamori/providers/heroku"

mamori.WithProvider(heroku.New(
	heroku.WithAPIKey(os.Getenv("HEROKU_API_KEY")),
	heroku.WithApp("my-app"),
))
```

| Option | Default | Effect |
| --- | --- | --- |
| `WithAPIKey` | `HEROKU_API_KEY` | The Heroku API token. An empty string is ignored, so it falls back to the environment rather than pinning an unusable credential. |
| `WithApp` | `HEROKU_APP`, then `HEROKU_APP_NAME` | The app for refs that name none themselves. |
| `WithBaseURL` | `https://api.heroku.com` | Point at a proxy or a test double. |
| `WithAllowInsecure` | off | Permits an `http://` base URL, and nothing else. |
| `WithHTTPClient` | default client | Supply your own `*http.Client`. |

The token and the app are read lazily at resolve time, so a blank import alone is enough once the environment is set, and a process whose credentials arrive after start still works.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Heroku. Without `WithHTTPClient` it releases nothing: this provider builds no default client of its own to hold onto. A client injected with `WithHTTPClient` is never closed: `Close` may return its idle connections to the pool, but leaves the client usable.

Verified against an in-process HTTP fake, so the conformance kit runs without a Heroku account. Nobody on this project has one, so the wire shape follows the published Platform API reference rather than a live capture, and the `//go:build integration` tests are how the first person with a token confirms it.
