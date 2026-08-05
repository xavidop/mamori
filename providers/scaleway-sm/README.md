# mamori - Scaleway Secret Manager provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves secrets from
[Scaleway Secret Manager](https://www.scaleway.com/en/secret-manager/), Scaleway's
regional secret store for API keys, database credentials, and other sensitive
values. Scaleway publishes a Go SDK (`scaleway-sdk-go`), but it is not used here:
the read path is a single authenticated GET against a documented HTTPS API, so this
provider uses `net/http` and the standard library only, keeping the SDK's
dependency tree, and its transitive requirements, out of every consumer's build.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/scaleway-sm"
```

Importing the package registers the `scaleway-sm` scheme with mamori. The
provider reads `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, and `SCW_DEFAULT_REGION`
lazily at resolve time, so it is safe to register from a blank import even when no
credentials exist at process start.

## Scheme

```
scaleway-sm://<name>                  secret at the root path, latest enabled revision
scaleway-sm://<path>/<name>           secret at an explicit path
scaleway-sm://<name>?revision=<n>     an explicit revision number (fails if disabled)
scaleway-sm://<name>?revision=latest  the newest revision (fails if it is disabled)
```

- `<name>` - the **last** path segment, always. Everything before it is `<path>`, a
  real slash-delimited directory Scaleway secrets are organized under (a secret's
  full location is effectively path plus name). This differs deliberately from
  [`providers/cloudflare-kv`](../cloudflare-kv/), where the **entire** ref path is
  one key: Workers KV keys may themselves legally contain slashes, so splitting
  there would silently misread a key like `config/prod/log-level` as a namespace
  plus a shorter key. Scaleway's path segments are real directories rather than
  characters inside the secret's own name, so splitting on the ref's last slash
  here is correct for this backend, not a bug to reconcile against that sibling.
- `?revision=<n|latest>` - optional. Defaults to `latest_enabled`, **not**
  `latest` - see [Revisions and the disabled state](#revisions-and-the-disabled-state)
  below for why that default was chosen deliberately.
- `#field` / `#/json/pointer` - optional. When present, the resolved bytes are
  parsed as JSON and the field is selected via `mamori.SelectKey` (identical to
  every other mamori provider): a fragment starting with `/` is an RFC 6901 JSON
  Pointer for nested selection (`#/retry/maxAttempts`); anything else is a literal
  top-level key (`#timeout`).

**Access by secret ID is not supported.** Scaleway's API also has a by-ID access
route, keyed on the secret's UUID rather than its path and name, and this provider
does not use it. This is a decision, not a gap: a UUID sitting in a struct tag's
`source:"..."` value is unreadable at the call site, and Scaleway's own path-based
naming is the ergonomic route it documents for exactly this kind of use. If you
only have a secret ID, look up its path in the Scaleway console or with `scw` first.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `scaleway-sm://db-password` | Latest enabled revision of `db-password` at the root path |
| `scaleway-sm://prod/db-password` | Latest enabled revision of `db-password` under path `/prod` |
| `scaleway-sm://a/b/c/secret` | Latest enabled revision of `secret` under path `/a/b/c` |
| `scaleway-sm://db-password?revision=7` | Revision `7` exactly - fails if revision `7` is disabled |
| `scaleway-sm://db-password?revision=latest` | The newest revision - fails if that revision is disabled |
| `scaleway-sm://api-config#timeout` | Field `timeout` of the JSON-valued secret `api-config` |

```go
type Config struct {
    DBPassword secret.String `source:"scaleway-sm://prod/db-password"`
    APIKey     secret.String `source:"scaleway-sm://api-key?revision=42"`
    Timeout    string        `source:"scaleway-sm://api-config#timeout"`
}
```

## Revisions and the disabled state

Disabling a revision is how a Scaleway operator revokes a leaked credential
without deleting its history. On Scaleway, disabling is an access revocation,
not a preference hint: `scaleway-sdk-go` documents a disabled `SecretVersion`
as "not accessible but can be enabled," and the Scaleway CLI names the
operation to match (`scw secret version disable`/`enable` = make a version
inaccessible/accessible). **A request for a disabled revision fails.** It does
not return that revision's bytes, whether the revision is named by number or
reached via `?revision=latest`.

That is why `?revision` defaults to `latest_enabled` rather than `latest`:
`latest_enabled` is the selector that keeps working across a revocation,
automatically falling back to the newest revision that is still enabled,
while `latest` breaks the moment the newest revision is disabled. Defaulting
to `latest` would not "keep serving a revoked secret" - it would serve
nothing at all the instant an operator revoked one.

Given a secret with revision 3 enabled and a newer revision 4 that has been
disabled:

| Ref | Resolves to |
| --- | --- |
| `scaleway-sm://cred` (no `?revision`) | Revision 3 - the newest **enabled** one |
| `scaleway-sm://cred?revision=latest` | **Fails** - revision 4 is the newest, but it is disabled and therefore inaccessible |
| `scaleway-sm://cred?revision=4` | **Fails** for the same reason: pinning the disabled revision's exact number does not help |

There is no escape hatch that returns the newest revision regardless of its
enabled state; Scaleway does not offer one. This interacts with [the 404
caveat](#error-classification) below: this API's 404 does not distinguish an
unknown secret from a disabled or nonexistent revision, so a ref that pins
`?revision=latest` and is later hit by a revocation degrades silently to the
field's default (or optional handling) instead of failing loudly - the same
silent-degradation hazard the 404 caveat already describes for a deleted
revision. `latest_enabled` avoids the failure in the first place, which is
the strongest reason to leave it as the default rather than pinning `latest`.

## Value mapping

- **`Value.Sensitive` is always `true`.** Unlike `providers/vercel-gc` and
  `providers/cloudflare-kv` in this same trio of recent additions, both of which
  read a general-purpose config or KV store and report `Sensitive: false`, this
  provider reads a real secret manager, so every resolved value is marked
  accordingly.
- **`Value.Version` is the backend revision, not a content hash.** It is
  `resp.Revision` rendered as a decimal string whenever the response reports one,
  which in practice is always, since Scaleway numbers revisions from 1. It falls
  back to `mamori.VersionHash` only if a response arrives with no `revision` at
  all, because a constant `Version` would make every later rotation invisible to
  mamori's poller. Either way it is never affected by a `#field` selection that
  narrows the returned bytes. This is
  the first provider in this trio of recent additions (alongside `providers/vercel-gc`
  and `providers/cloudflare-kv`) that can do this: the other two read a
  general-purpose config or KV store and fall back to a content hash because their
  backend exposes no revision, which makes two byte-identical values at two
  different points in time indistinguishable to them. (Reporting a real backend
  version is not unique to this trio across the wider repo - `providers/aws`,
  `providers/gcp`, `providers/azure`, `providers/vault`, `providers/k8s`, and
  `providers/onepassword` all do it too, as do `providers/etcd` and
  `providers/consul`, which are general-purpose stores that happen to expose one
  anyway - but within this trio it is new.) A real secret manager does not have
  the general-purpose store's excuse - the revision already identifies exactly
  which write produced these bytes - so change detection here does not depend on
  comparing bytes at all: a rewrite that happens to produce the same bytes as
  before still advances `Version`, and mamori's poller will correctly treat it as
  a change. A `#field` selection changes which bytes are returned, not which
  secret version they came from, so `Version` stays the revision of the
  underlying secret even when the resolved payload is only part of it.
- **`Value.Metadata` carries `region` and `revision`, and nothing else** - not the
  secret id, not the project id, not the path, and never the value. This is
  deliberate: a secret's location is itself information, and `Metadata` reaches
  the admin HTTP endpoint and the status report, both broader-audience surfaces
  than "whoever holds the resolved value".
- **CRC verification** runs whenever the access response carries a `data_crc32`:
  the resolved bytes are checked against it with `crc32.ChecksumIEEE`, and a
  mismatch fails with `mamori.ErrInvalid`. Its absence is normal, not an error -
  Scaleway populates `data_crc32` only when a CRC was supplied at write time, so a
  secret written without one resolves exactly as if no verification had been
  requested at all.

## No `ResolveBatch`

This provider deliberately does not implement `mamori.BatchProvider`. Secret
Manager's access-secret-version endpoint returns one revision of one secret;
there is no bulk endpoint that returns many secrets' payloads in a single call.
A `ResolveBatch` here would just be a loop over `Resolve` internally, claiming a
round-trip saving that the API does not actually deliver, so each `Resolve` costs
its own request rather than pretending otherwise.

There is also no cache and no TTL: `mamori.Refresh` and `mamori.Doctor` both call
`Resolve` directly, this provider holds no snapshot between calls to gate a cache
on, and every call is a live GET against the current revision.

## Error classification

A 404 is detected before status classification runs and reports
`mamori.ErrNotFound` directly. Every other non-2xx response is classified by HTTP
status:

| HTTP status | mamori kind |
| --- | --- |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 408, 429 | `rate_limited` |
| 400, 422 | `invalid` |
| 5xx, and any other status not named above | `unavailable` |

The mapping is `httpcore.ClassifyStatus`, shared with every other
`httpcore`-backed provider, rather than a table private to this module. An
unrecognized status reports `unavailable` (transient, so mamori backs off and
retries) rather than `unknown`.

**The 404 caveat.** Scaleway's by-path access route returns the same 404, with no
distinguishing error code in the body, whether the secret name is unknown, the
secret is known but the requested revision does not exist, or the requested
revision is known but **disabled** (see [Revisions and the disabled
state](#revisions-and-the-disabled-state) above - disabling makes a revision
inaccessible, not merely non-default). A ref asking for `?revision=99` against a
secret that has only ever reached revision 12 gets the same 404 an entirely
absent secret would, and so does a ref pinning `?revision=latest` after an
operator disables the newest revision. Either way it degrades silently to the
field's default or optional handling, exactly as if the secret had never existed
at all. Scaleway has not published a stable enough error-code vocabulary in the
response body to key this mapping on anything but the status, so codes not
listed here report `unavailable` rather than being guessed at.

## Authentication & configuration

Reading a secret requires a Scaleway API secret key, a project id, and a region.
Each may be set explicitly or read from the environment; an explicit option wins
over its environment variable:

| Source | Option | Environment variable |
| --- | --- | --- |
| API secret key | `WithSecretKey(key)` | `SCW_SECRET_KEY` |
| Project id | `WithProjectID(id)` | `SCW_DEFAULT_PROJECT_ID` |
| Region | `WithRegion(region)` | `SCW_DEFAULT_REGION` (falls back to `fr-par` when neither is set) |

The environment variable names are Scaleway's own, the same ones `scw` (the
official CLI) and Scaleway's Terraform provider already read, so a machine
already configured for either works unchanged with this provider - no
mamori-specific renaming to redo.

```go
import scalewaysm "github.com/xavidop/mamori/providers/scaleway-sm"

mamori.WithProvider(scalewaysm.New(
    scalewaysm.WithSecretKey(os.Getenv("SCW_SECRET_KEY")),
    scalewaysm.WithProjectID(os.Getenv("SCW_DEFAULT_PROJECT_ID")),
))
```

If the secret key or the project id is missing when a ref is resolved, `Resolve`
returns an error naming both the option and the environment variable that would
supply it, and never echoes a credential that is set. The request itself
authenticates via the `X-Auth-Token` header, never a query parameter, so a
resolved value's request cannot leak the secret key into a log line or an error
message.

### Options

| Option | Effect |
| --- | --- |
| `WithSecretKey(key)` | Set the Scaleway API secret key used to authenticate requests |
| `WithProjectID(id)` | Set the Scaleway project id that owns the secrets |
| `WithRegion(region)` | Set the region Secret Manager requests are sent to |
| `WithBaseURL(u)` | Override the API origin, for an `httptest.Server` or a proxy; a trailing slash is trimmed so joining it with a path never produces a double slash |
| `WithHTTPClient(c)` | Inject a custom `*http.Client`; a nil client is a no-op |

`Close()` is idempotent and terminal: after it returns, every `Resolve`
reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting
Secret Manager. It also returns its own idle HTTP connections to the pool, and
leaves connections belonging to the rest of your process alone. A client
injected with `WithHTTPClient` is never closed, so it stays usable for
whatever else holds it.

## No native watch

The Secret Manager REST API exposes no streaming or blocking read, so this
provider deliberately does not implement `mamori.WatchableProvider`, and mamori
wraps it in the polling adapter instead, using `Value.Version` (the real backend
revision, see above) to detect a change between ticks.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake HTTP transport (`go test ./...`), never a real `httptest.Server` |
| Ref parsing: last-segment split into path/name, default revision `latest_enabled`, explicit `?revision=<n>`/`latest`, a `#fragment` is not part of the name, error cases (empty name, trailing slash) | **Verified** (unit tests) |
| Settings precedence (explicit option over environment), region falling back to `fr-par`, and errors that name the missing option without ever echoing a set secret key or project id | **Verified** (unit tests) |
| `Value.Version` is the real backend revision, not a content hash: two revisions carrying byte-identical payloads report different `Version`, and a `#field` selection narrows `Bytes` without changing `Version` | **Verified** (`TestResolveVersionIsRevisionNotContentHash`, `TestResolveVersionStaysRevisionEvenWithFieldSelection`) |
| `Value.Version` falls back to a content hash if a response ever carries no revision, so a constant `Version` cannot make later rotations invisible to the poller | **Verified** (`TestValueForVersionFallsBackToContentHashWhenRevisionAbsent`) |
| `Value.Sensitive` is `true`, and wrapping the resolved bytes in `secret.String` redacts under `fmt` | **Verified** (`TestResolveValueSensitiveAndRedactsViaSecretString`) |
| `Value.Metadata` carries exactly `region` and `revision` - never the secret id, project id, path, or value | **Verified** (`TestResolveMetadataOnlyRegionAndRevision`) |
| CRC verification: a matching `data_crc32` resolves, a mismatch fails with `mamori.ErrInvalid`, and an absent `data_crc32` is not an error | **Verified** (unit tests) |
| The disabled-revision decision: `?revision` defaulting to `latest_enabled` skips a disabled revision and resolves the newest enabled one; a disabled revision fails to resolve regardless of whether it is addressed via `latest` or by its exact number | **Verified** (`TestResolveLatestEnabledSkipsDisabledRevision`, `TestResolveDisabledRevisionFails`) |
| `#field` and `#/json/pointer` selection, including a nested pointer | **Verified** (unit tests) |
| Not-found (unknown secret) | **Verified** (`TestResolveUnknownSecretIsNotFound`) |
| Error classification (401/403/400/422/408/429, and an unnamed status such as 418 as `unavailable`), exercised through `Resolve` | **Verified** (unit tests + `providertest` `ErrorClassification` case) |
| Credentials never reach an error: the secret key and project id never appear in an error string, from a live transport failure or a malformed `WithBaseURL` | **Verified** (`TestResolveTransportErrorNeverLeaksCredentials`, `TestResolveMalformedBaseURLNeverLeaksCredentials`) |
| No cache: repeated resolves of an unchanged secret cost one request each, and a failure injected between resolves is observed on the very next call | **Verified** (`TestResolveNeverCaches`) |
| Context cancellation | **Verified** (`TestResolveHonorsContextCancellation`) |
| `BatchProvider` is deliberately not implemented; `WatchableProvider` is deliberately not implemented | **Verified** (`TestProviderIsNotBatchable`, `TestProviderIsNotWatchable`) |
| End-to-end against a real Secret Manager project, including that `Version` really does track the backend revision | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that models multiple
numbered revisions per secret with independent enabled/disabled state (not just a
single canned response), because the disabled-revision behavior above can only be
tested honestly against a fake that actually models revision history. `go test
./...` requires **no** network access and **no** Scaleway credentials.

### Live integration test

An integration test exercises a real Secret Manager project. It is guarded by a
build tag and skips unless `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, and
`SCALEWAY_SM_TEST_SECRET` (the name of an existing secret) are set. It cannot
create secrets - that would need Scaleway's management API - so it verifies the
read path, and it is also the only way to confirm that a real Secret Manager
response's `revision` field means what `valueFor` assumes it means, which a fake
can only confirm agreement with its own bookkeeping of:

```sh
export SCW_SECRET_KEY=...
export SCW_DEFAULT_PROJECT_ID=...
export SCALEWAY_SM_TEST_SECRET=some-existing-secret-name
GOWORK=off go test -tags integration -run Integration ./...
```

`SCW_DEFAULT_REGION` is honored if set, but not required: Secret Manager falls
back to `fr-par` the same way the non-integration path does.

## Development

This provider is its own Go module. Run all commands with the workspace
disabled:

```sh
cd providers/scaleway-sm
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
