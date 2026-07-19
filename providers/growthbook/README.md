# mamori GrowthBook provider

`github.com/xavidop/mamori/providers/growthbook`

A [mamori](https://github.com/xavidop/mamori) provider for
**[GrowthBook](https://www.growthbook.io/)** feature flags. It registers the
`growthbook` scheme and resolves the evaluated value of a GrowthBook feature
(optionally selecting a JSON field).

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against an
in-memory fake. See [Verified vs. needs a live backend](#verified-vs-needs-a-live-backend).

## Install

```bash
go get github.com/xavidop/mamori/providers/growthbook
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/growthbook"
```

The package identifier is `growthbook`.

## Scheme

```
growthbook://<feature-key>[#json-key]
```

| Part            | Meaning                                                              |
| --------------- | ------------------------------------------------------------------- |
| `<feature-key>` | The id of a feature in your GrowthBook project.                     |
| `#json-key`     | Optional. Select a single field from a JSON-object feature value.  |

The provider loads the current feature set (from the GrowthBook Features API, or
from an offline features JSON) and evaluates the named feature with the
[GrowthBook Go SDK](https://github.com/growthbook/growthbook-golang). The
feature's evaluated value is returned encoded as bytes:

| Feature value type | Returned bytes                    |
| ------------------ | --------------------------------- |
| boolean            | `true` / `false`                  |
| string             | the raw string                    |
| number             | its decimal string form (`25`)    |
| object / array     | its JSON encoding                 |
| null               | `null`                            |

When `#json-key` is present, `mamori.SelectKey` is applied to the (JSON) feature
value, identically to every other mamori provider: string fields come back
unquoted, other fields as their JSON encoding.

> Evaluation uses **no user attributes**, so a feature resolves to the value it
> takes for an anonymous/empty evaluation context (its default value, or the
> first force rule with no targeting condition). This suits configuration-style
> flags. Percentage rollouts and attribute-targeted experiments are not
> meaningfully resolvable without a user context and are outside this provider's
> scope.

## Ref examples

```go
type Config struct {
    // A whole feature value:
    DarkMode string `source:"growthbook://dark_mode"`

    // Numeric / boolean values come back as their string / JSON form:
    MaxItems int `source:"growthbook://max_items"`

    // Select a field from a JSON feature value:
    NewUIEnabled string `source:"growthbook://feature_flags#new_ui"`
}
```

Resolved values are **not** marked sensitive (`Value.Sensitive == false`) -
GrowthBook features are configuration / feature flags, not secrets.

`Value.Version` is `mamori.VersionHash` of the evaluated feature value, so it
changes whenever the feature's value changes, giving mamori cheap change
detection.

A feature that is **not present** in the loaded feature set (the GrowthBook SDK
reports it as an unknown feature), and an absent `#json-key`, resolve to an error
satisfying `errors.Is(err, mamori.ErrNotFound)`, so mamori applies your default /
optional handling.

## Polling (no native watch)

This provider is **not watchable**. GrowthBook offers a streaming (SSE)
endpoint, but this provider deliberately does not implement native `Watch`:
mamori polls it on the configured interval, re-fetching the feature set on each
poll. This keeps the provider free of background goroutines. Do not expect push
updates.

## Authentication and configuration

### API-backed (GrowthBook Cloud or self-hosted)

Supply your **SDK client key** and, for a self-hosted GrowthBook, its **API
host**. No network access happens at registration time; the SDK client is
created lazily on the first `Resolve`.

```go
import (
    "github.com/xavidop/mamori"
    "github.com/xavidop/mamori/providers/growthbook"
)

mamori.WithProvider(growthbook.New(
    growthbook.WithClientKey("sdk-abc123"),
    growthbook.WithAPIHost("https://cdn.growthbook.io"), // optional; this is the SDK default
))
```

- `WithClientKey(key)` - the GrowthBook SDK client key (required for API-backed
  operation). The feature set is fetched from `<api-host>/api/features/<key>`.
- `WithAPIHost(host)` - the GrowthBook API host. Defaults to
  `https://cdn.growthbook.io` (GrowthBook Cloud); set it to your self-hosted
  GrowthBook API URL when self-hosting.
- `WithDecryptionKey(key)` - decrypt an encrypted feature payload.
- `WithHTTPClient(c)` - custom `*http.Client` (proxy, custom transport, tests).

### Offline (air-gapped / pinned feature set)

Supply the raw features JSON (the `features` object of a GrowthBook SDK payload)
with `WithFeatures`. In this mode the provider performs **no network access**.

```go
const features = `{
    "dark_mode":     {"defaultValue": true},
    "welcome":       {"defaultValue": "Hello!"},
    "feature_flags": {"defaultValue": {"new_ui": true, "label": "beta"}}
}`

mamori.WithProvider(growthbook.New(growthbook.WithFeatures(features)))
```

Combine `WithFeatures` with `WithDecryptionKey` to supply an encrypted payload.

## Verified vs. needs a live backend

**Verified in unit tests (no GrowthBook access):** scheme and registration, ref
parsing (`<feature-key>`, `#json-key`), value encoding for booleans, strings,
numbers, objects, arrays and null, `#json-key` selection, `Sensitive == false`,
`mamori.VersionHash` versioning and change-on-mutate, unknown-feature and absent
`#json-key` -> `mamori.ErrNotFound`, empty-key and missing-configuration
handling, context cancellation, concurrent resolution, and the full
`providertest.Run` conformance suite (with `SkipWatch`, since the provider is
non-watchable) against an in-memory fake.

The **real SDK** evaluation path is verified two ways without a live backend:

- **Offline** feature set (`WithFeatures`): real SDK JSON decoding and
  evaluation of booleans, strings, numbers and JSON objects.
- **API-backed** path against an `httptest` server serving a GrowthBook Features
  API response: the fetch URL (`/api/features/<client-key>`), JSON decoding,
  evaluation, non-200 error handling, and unknown-feature -> `ErrNotFound`.

**Needs a live backend (not run in CI):** a real GrowthBook client key / project,
real HTTPS transport to `cdn.growthbook.io` (or a self-hosted API), and real
feature-set data. These are exercised by the build-tagged live test in
`growthbook_integration_test.go`:

```bash
GROWTHBOOK_CLIENT_KEY=sdk-abc123 \
GROWTHBOOK_API_HOST=https://cdn.growthbook.io \
GROWTHBOOK_FEATURE=my_feature \
GROWTHBOOK_EXPECT=true \
go test -tags integration -run TestLive ./...
```

The feature named by `GROWTHBOOK_FEATURE` must exist in the project's feature
set. Both live tests skip automatically when the env vars are unset.

## Development

```bash
cd providers/growthbook
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
