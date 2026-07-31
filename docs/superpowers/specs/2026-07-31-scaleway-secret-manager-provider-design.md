# Scaleway Secret Manager provider design

**Status:** approved
**Date:** 2026-07-31

Adds `providers/scaleway-sm`, a provider resolving secrets from
[Scaleway Secret Manager](https://www.scaleway.com/en/secret-manager/) over its
REST API, using the standard library only.

This is the third module in a stack whose earlier members are
[`providers/vercel-gc`](2026-07-31-vercel-global-config-provider-design.md) and
[`providers/cloudflare-kv`](2026-07-31-cloudflare-kv-provider-design.md). It
follows their conventions; what follows is what genuinely differs.

## Why this backend, and why it is the most interesting of the three

The other two modules in this stack read general-purpose config stores. This
one reads a **real secret manager**, which changes three things:

1. **`Value.Sensitive` is `true`.** This is the first of the three that is
   secret-bearing, so values redact in `fmt`, JSON, and `slog` by default.
2. **`Value.Version` is a genuine backend revision.** Scaleway numbers versions
   from 1 and increments on every write, and the access response returns that
   number. Every other provider in this stack falls back to
   `mamori.VersionHash` because its backend exposes no revision. Here mamori
   gets what its `Value.Version` field was actually designed for: cheap,
   correct change detection that does not depend on comparing bytes.
3. **Rotation is first-class.** Because revisions are real and addressable, a
   ref can pin `?revision=` to an exact number, or track `latest_enabled`, and
   `PreApply` can prove a rotated credential works before it goes live.

Scaleway publishes a Go SDK. It is deliberately not used: the read path is one
authenticated GET, and pulling `scaleway-sdk-go` in would put its whole
dependency tree into a user's build. The module needs `net/http` and nothing
else. All API details below were verified against the SDK's own source rather
than inferred from prose documentation.

## What this is not

- **Not watchable.** Secret Manager exposes no streaming read, no blocking
  read, and no change feed. Per
  [writing-a-provider/capabilities](../../../site/src/pages/docs/writing-a-provider/capabilities.md),
  a provider must never fake a `Watch` with an internal ticker, so this
  provider does not implement `WatchableProvider` and mamori drives it with the
  polling adapter.
- **Not a cache.** Every `Resolve` fetches. Unlike `vercel-gc` there is no
  digest to gate a snapshot on, and holding secrets in a provider-level cache
  would extend how long secret material stays in process memory for no gain.
- **Not batchable.** The API accesses one secret version per request; there is
  no bulk endpoint. So this module deliberately does **not** implement
  `BatchProvider`, and mamori falls back to individual `Resolve` calls. Adding
  a `ResolveBatch` that loops internally would be a lie: it would claim a round
  trip saving that does not exist.
- **Not a write path.** mamori is not a store.

## Scheme and ref grammar

```
scaleway-sm://<name>                      secret at the root path
scaleway-sm://<path>/<name>               secret at /<path>
scaleway-sm://<name>?revision=7           pin an exact revision
scaleway-sm://<name>?revision=latest      newest revision (fails if it is disabled)
scaleway-sm://<name>#user                 select a field of a JSON secret
scaleway-sm://<name>#/db/password         RFC 6901 pointer selection
```

```go
type Config struct {
    DBPassword secret.String `source:"scaleway-sm://prod/db-password"`
    APIKey     secret.String `source:"scaleway-sm://stripe-key?revision=latest"`
    DBUser     string        `source:"scaleway-sm://prod/db#username"`
    Pinned     secret.String `source:"scaleway-sm://legacy-token?revision=3"`
}
```

Secrets in Scaleway live at a path, and every path is prefixed with a slash. A
secret named `db-password` at path `/prod` is addressed as
`scaleway-sm://prod/db-password`: the **last** path segment is the secret name
and everything before it is the path. A single segment means the root path `/`.

This is unambiguous, and unlike `providers/cloudflare-kv` it is safe to split on
slashes, because Scaleway's path is itself a slash-delimited directory
structure rather than an opaque key. The two modules diverge here for a real
reason, and both READMEs should say so, since a reader moving between them will
otherwise assume one of them is wrong.

**Access by secret ID is not supported.** The API offers it, but a UUID in a
struct tag is unreadable and the by-path route is the ergonomic one Scaleway
itself documents. This is a deliberate omission, stated in the README so it
reads as a decision rather than a gap.

### Revision

`?revision=` accepts what the API accepts: a number, `latest`, or
`latest_enabled`. **The default is `latest_enabled`**, not `latest`.

That default is the load-bearing choice in this design. Disabling a revision
is exactly how an operator revokes a leaked credential, and on Scaleway that
makes the revision INACCESSIBLE, not merely non-default: a request for a
disabled revision fails outright, whether it is addressed by its exact number
or reached via `latest`. So `latest` does not survive a revocation - it
breaks the instant the newest revision is disabled - while `latest_enabled`
is the selector that keeps working, automatically resolving the newest
revision that is still enabled. Defaulting to `latest` would not "keep
serving a secret that has been explicitly disabled" - it would keep serving
nothing at all - which is why `latest_enabled`, not `latest`, is the default.

## Authentication and configuration

Scaleway authenticates with an API secret key in an `X-Auth-Token` header.

| Setting | Option | Environment | Default |
| --- | --- | --- | --- |
| API secret key | `WithSecretKey` | `SCW_SECRET_KEY` | none, required |
| Project ID | `WithProjectID` | `SCW_DEFAULT_PROJECT_ID` | none, required |
| Region | `WithRegion` | `SCW_DEFAULT_REGION` | `fr-par` |

Plus `WithBaseURL` for an `httptest.Server` or a proxy, and `WithHTTPClient`.
The environment variable names are Scaleway's own, so a machine already
configured for `scw` or Terraform works with no extra setup.

Valid regions are `fr-par`, `nl-ams`, and `pl-waw`, confirmed from the SDK.
The provider does not validate the region against that list: Scaleway adds
regions, and a hardcoded allowlist would reject a valid new one. An unknown
region fails at the API with a classified error, which is the honest outcome.

**No credential, project ID, or resolved value may reach an error message, a
log line, or `Value.Metadata`.** Both earlier modules in this stack shipped a
leak of exactly this kind: `url.Parse` and `http.Client.Do` both return
`*url.Error` whose message embeds the entire request URL. This module's request
URLs carry the project ID as a query parameter, so the same guard is required
from the start rather than added in review.

## Resolve

```
GET {base}/secret-manager/v1beta1/regions/{region}/secrets-by-path/versions/{revision}/access
    ?secret_path={path}&secret_name={name}&project_id={project}
X-Auth-Token: {secret key}

-> {"secret_id": "...", "revision": 3, "data": "<base64>", "data_crc32": 123, "type": "opaque"}
```

- **`data` is base64 in the JSON and decodes into a Go `[]byte` field for
  free.** `encoding/json` unmarshals a base64 string into `[]byte`
  automatically, so no manual decoding step is needed and none should be added.
- `Value.Bytes` is the decoded payload. `#key` selection then applies via
  `mamori.SelectKey`, matching every other provider.
- **`Value.Version` is the response's `revision`, rendered as a decimal
  string.** Not a content hash. This is the point of the module.
- `Value.Sensitive` is **`true`**.
- `Metadata` carries the region and the revision. **Never** the secret id, the
  project id, the path, or the value: a secret's location is itself
  information, and `Metadata` reaches the admin endpoint and the status report.

### `data_crc32` is verified when present

The response optionally carries a CRC32 of the payload. When present, the
provider verifies it and returns an error wrapping `mamori.ErrInvalid` on
mismatch. Scaleway populates it only when a CRC was supplied at write time, so
absence is normal and not an error.

This is cheap and it is the only integrity signal the API offers. A silently
truncated secret that still parses is a genuinely nasty failure, and for a
credential it means an authentication failure at some later, less obvious
moment.

## Error classification

| Status | Kind |
| --- | --- |
| 401 | `ErrUnauthenticated` |
| 403 | `ErrPermissionDenied` |
| 404 | `ErrNotFound` (unknown secret, path, or revision) |
| 429 | `ErrRateLimited` |
| 400 | `ErrInvalid` |
| 5xx | `ErrUnavailable` |
| anything else | unclassified |

404 is handled before classification, on the single code path that exists.
Unlike `providers/cloudflare-kv`, there is no second endpoint for a 404 branch
to be forgotten on, which is the defect that module's final review caught.

One caveat belongs in the code and the README: a 404 does not distinguish an
unknown secret from a known secret whose requested revision does not exist, so
`?revision=99` on a secret with three revisions reports not-found and the field
takes its default. Pinning a revision that is later deleted therefore degrades
silently to the default rather than failing loudly.

**A disabled revision is not an error.** With the default `latest_enabled`, a
disabled newest revision means the previous enabled one is served. That is the
intended behavior and the README should show it, because an operator disabling
a revision expects exactly this.

## Testing

Mirrors the earlier modules, including the constraint that shaped both:
`providertest`'s `NoGoroutineLeak` runs `goleak.VerifyNone` with **no ignore
options**, so the fake must be driven through an in-process `RoundTripper` and
never a live `httptest.Server`.

| Aspect | How |
| --- | --- |
| `providertest.Run` conformance | Against an `httptest`-free fake, with `Fail` honoring the injected sentinel so every case round-trips through the real classifier |
| Ref parsing | root-path and nested-path forms, an empty name, and a trailing slash |
| Revision | default is `latest_enabled`; `latest` and a pinned number reach the URL; a disabled newest revision serves the previous enabled one |
| Version is the revision | a bytes-identical value at a new revision still reports a changed `Version`, which a content hash could not do |
| base64 | a payload with non-UTF8 bytes round-trips exactly |
| CRC | a mismatched `data_crc32` yields `ErrInvalid`; an absent one is not an error |
| Sensitive | `Value.Sensitive` is true, and a `secret.String` field redacts |
| Credential safety | no error text contains the secret key or the project id, pinned at **every** `sanitize` call site, not one |
| Metadata | contains region and revision, and does **not** contain the secret id, project id, path, or value |
| Not batchable | `New()` does not satisfy `mamori.BatchProvider`, asserted deliberately |
| Live backend | build-tagged, skipped unless `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, and a test secret name are set |

The credential-safety test covering every `sanitize` call site rather than one
is a direct lesson from `providers/cloudflare-kv`, where 1 of 4 sites was
pinned and removing any of the other three left the whole suite green.

## Documentation

Shipping with the code: `providers/scaleway-sm/README.md`,
`site/src/pages/docs/providers/scaleway-sm.md` plus its navigation entry under
"Secret managers" rather than "KV & config", the site provider matrix row, the
root `README.md` table row and install line, the `skills/mamori` reference
entry, and **the `.github/dependabot.yml` entry that CI enforces**.
