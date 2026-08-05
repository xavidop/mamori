---
layout: ../../../layouts/DocsLayout.astro
title: Scaleway Secret Manager
---

# Scaleway Secret Manager

Load a secret from [Scaleway Secret Manager](https://www.scaleway.com/en/secret-manager/), Scaleway's regional secret store for API keys, database credentials, and other sensitive values. Pure `net/http`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `scaleway-sm://` |
| Module | `github.com/xavidop/mamori/providers/scaleway-sm` |
| Sensitive | yes |
| Watch | poll |
| Auth | `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, `SCW_DEFAULT_REGION` |

## Install

```bash
go get github.com/xavidop/mamori/providers/scaleway-sm
```

```go
import _ "github.com/xavidop/mamori/providers/scaleway-sm"
```

## Using the ref

A `scaleway-sm://` ref points at one secret, addressed by its path and name, optionally pinning a revision and selecting a field from a JSON-valued secret.

```text
scaleway-sm://<name>
scaleway-sm://<path>/<name>
scaleway-sm://<name>?revision=<n|latest>
scaleway-sm://<name>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<name>` | yes | The **last** path segment, always. |
| `<path>` | no | Everything before the last segment - a real slash-delimited directory Scaleway secrets are organized under, not part of the secret's own name. |
| `?revision=<n\|latest>` | no | Defaults to `latest_enabled`; `latest` requests the newest revision but fails if that revision has been disabled (see below). |
| `#field` / `#/json/pointer` | no | Select a field from a JSON-valued secret via `mamori.SelectKey` - a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `scaleway-sm://db-password` reads the latest enabled revision of `db-password` at the root path.
- `scaleway-sm://prod/db-password` reads the same secret under path `/prod`.
- `scaleway-sm://db-password?revision=7` pins revision `7` exactly.
- `scaleway-sm://api-config#timeout` reads field `timeout` of the JSON-valued secret `api-config`.

```go
type Config struct {
	DBPassword secret.String `source:"scaleway-sm://prod/db-password"`
	Timeout    string        `source:"scaleway-sm://api-config#timeout"`
}
```

The **last** path segment is always the secret name; everything before it is the path. This is the opposite split from [Cloudflare Workers KV](/docs/providers/cloudflare-kv/), where the entire ref path is one key: Workers KV keys may themselves legally contain slashes, so splitting there would silently misread a key like `config/prod/log-level`. Scaleway's path segments are real directories, so splitting on the last slash here is correct for this backend rather than a bug to reconcile against that sibling.

Access by secret ID (Scaleway's UUID) is not supported - a UUID in a struct tag is unreadable at the call site, and the by-path route is the ergonomic one Scaleway itself documents.

## Revisions and the disabled state

Disabling a revision is how a Scaleway operator revokes a leaked credential without deleting its history. On Scaleway, disabling is an access revocation, not a preference hint: `scaleway-sdk-go` documents a disabled `SecretVersion` as "not accessible but can be enabled," and the Scaleway CLI names the operation to match (`scw secret version disable`/`enable` = make a version inaccessible/accessible). **A request for a disabled revision fails** - it does not return that revision's bytes, whether the revision is named by number or reached via `?revision=latest`. There is no escape hatch that returns the newest revision regardless of its enabled state; Scaleway does not offer one.

That is why `?revision` defaults to `latest_enabled` rather than `latest`: `latest_enabled` is the selector that keeps working across a revocation, automatically falling back to the newest revision that is still enabled, while `latest` breaks the moment the newest revision is disabled. Defaulting to `latest` would not "keep serving a revoked secret" - it would serve nothing at all the instant an operator revoked one.

Given a secret with revision 3 enabled and a newer revision 4 disabled:

| Ref | Resolves to |
| --- | --- |
| `scaleway-sm://cred` | Revision 3 - the newest **enabled** revision |
| `scaleway-sm://cred?revision=latest` | **Fails** - revision 4 is the newest, but it is disabled and therefore inaccessible |
| `scaleway-sm://cred?revision=4` | **Fails** for the same reason: pinning the exact number of a disabled revision does not help |

This interacts with [the 404 caveat](#error-classification) below: a 404 from this API does not distinguish an unknown secret from a disabled or nonexistent revision, so a ref that pins `?revision=latest` and is later hit by a revocation degrades silently to the field's default (or optional handling) instead of failing loudly - the same silent-degradation hazard the 404 caveat describes for a deleted revision. `latest_enabled` avoids the failure in the first place, which is the strongest reason to leave it as the default rather than pinning `latest`.

## Value mapping

`Value.Sensitive` is always `true` - unlike [Vercel Global Config](/docs/providers/vercel-gc/) and [Cloudflare Workers KV](/docs/providers/cloudflare-kv/), which read general-purpose config/KV stores, this provider reads a real secret manager.

`Value.Version` is the backend revision itself (`resp.Revision` as a decimal string), not a content hash, falling back to `mamori.VersionHash` only if a response ever arrives carrying no revision at all - the first provider in this trio of recent additions (with [Vercel Global Config](/docs/providers/vercel-gc/) and [Cloudflare Workers KV](/docs/providers/cloudflare-kv/)) that can report a real revision rather than falling back to `mamori.VersionHash`; the other two read a general-purpose store with no revision to report. That means change detection here does not depend on comparing bytes: a rewrite that happens to produce the same bytes as before still advances `Version`, so mamori's poller correctly treats it as a change. A `#field` selection narrows `Value.Bytes` but never changes `Version`, since it tracks which write produced the secret, not which slice of its JSON was returned.

`Value.Metadata` carries only `region` and `revision` - never the secret id, project id, path, or value. A secret's location is itself information, and `Metadata` reaches the admin HTTP endpoint and the status report.

When the access response carries a `data_crc32`, the resolved bytes are checked against it and a mismatch fails with `mamori.ErrInvalid`. Its absence is normal, not an error: Scaleway populates `data_crc32` only when a CRC was supplied at write time.

## No `ResolveBatch`

Secret Manager's access-secret-version endpoint returns one revision of one secret; there is no bulk endpoint that returns many secrets in a single call. A `ResolveBatch` here would just be a loop over `Resolve`, claiming a round-trip saving the API does not actually deliver, so this provider deliberately does not implement `mamori.BatchProvider` and each `Resolve` costs its own request.

## Error classification

A 404 is detected before status classification runs and reports `not_found` directly. Every other failure is classified by HTTP status:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 408, 429 | `rate_limited` |
| 400, 422 | `invalid` |
| 5xx, and any other status not named above | `unavailable` |

The mapping is `httpcore.ClassifyStatus`, shared with every other `httpcore`-backed provider. An unrecognized status reports `unavailable` (transient, so mamori backs off and retries) rather than `unknown`.

An unknown secret, a known secret whose pinned revision does not exist, and a known secret whose pinned revision is **disabled** all return the identical 404, with no error code in the body to tell them apart. A ref asking for `?revision=99` against a secret that has only reached revision 12 degrades silently to the field's default, exactly as if the secret had never existed at all - and so does a ref pinning `?revision=latest` after an operator disables the newest revision.

## Watch

The Secret Manager REST API exposes no streaming or blocking read, so this provider does not implement `WatchableProvider` - mamori polls it instead (`WithPollInterval` + jitter), using the real backend revision above as `Version` to detect a change between ticks.

## Configuration

```go
import scalewaysm "github.com/xavidop/mamori/providers/scaleway-sm"

mamori.WithProvider(scalewaysm.New(
	scalewaysm.WithSecretKey(os.Getenv("SCW_SECRET_KEY")),
	scalewaysm.WithProjectID(os.Getenv("SCW_DEFAULT_PROJECT_ID")),
))
```

Reading a secret requires an API secret key, a project id, and a region. Each may be set explicitly (`WithSecretKey`, `WithProjectID`, `WithRegion`) or read from the environment (`SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, `SCW_DEFAULT_REGION`), read lazily at resolve time so a blank import alone is enough once the environment is set. An explicit option wins over its environment variable; the region additionally falls back to `fr-par` when neither is set. These are Scaleway's own environment variable names - the same ones `scw` and Scaleway's Terraform provider read - so a machine already configured for either works unchanged. The request authenticates via the `X-Auth-Token` header, never a query parameter.

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Secret Manager. It also returns its own idle HTTP connections to the pool, and leaves connections belonging to the rest of your process alone. A client injected with `WithHTTPClient` is never closed, so it stays usable for whatever else holds it.

Verified with an in-memory fake HTTP transport that models multiple numbered revisions per secret with independent enabled/disabled state (value mapping, the disabled-revision default, CRC verification, error classification, credential hygiene), so the conformance kit runs without a live Scaleway project. A `//go:build integration` test exercises a real project when `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, and `SCALEWAY_SM_TEST_SECRET` are set.
