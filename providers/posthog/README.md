# mamori - PostHog

Resolve mamori configuration values from [PostHog](https://posthog.com) feature
flags.

```go
import _ "github.com/xavidop/mamori/providers/posthog"

type Config struct {
    NewCheckout bool   `source:"posthog://new-checkout"`
    Pricing     string `source:"posthog://pricing-test#variant"`
    Limits      string `source:"posthog://pricing-test#payload"`
}
```

## Install

```sh
go get github.com/xavidop/mamori/providers/posthog
```

## The ref

```text
posthog://<flag-key>[#enabled | #variant | #payload]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<flag-key>` | yes | The feature flag's key in the PostHog project. |
| `#enabled` | no | The flag's enabled state, as `"true"` or `"false"`. |
| `#variant` | no | A multivariate flag's variant key. Empty for a boolean flag. |
| `#payload` | no | The flag's payload. Empty when the flag has none. |

Any other fragment is rejected with `mamori.ErrInvalid` rather than resolving to
an empty string, so a typo fails loudly at `mamori doctor` time.

### Where the distinct id goes

Nowhere in the ref. PostHog evaluates flags **for a distinct id**, and a distinct
id identifies the evaluation *context*, not the flag, so it is provider-level
configuration:

```go
mamori.WithProvider(posthog.New(
    posthog.WithProjectAPIKey(os.Getenv("POSTHOG_PROJECT_API_KEY")),
    posthog.WithDistinctID("svc-billing"),
))
```

Every ref this provider resolves is evaluated for that one subject. Putting the
id in each ref would repeat the same deployment identity across every field and
let two fields silently disagree about who they are asking about. This follows
`providers/launchdarkly`, whose evaluation context is likewise a provider option
(`WithContextKey`) with the same `"mamori"` default, for the same reason.

The default is `mamori`. A **stable** id matters more than a memorable one: a
percentage rollout hashes the distinct id, so a random id per process would put
two replicas of the same service on opposite sides of the same 50% rollout.

## Value mapping

PostHog's own SDKs return a boolean for a boolean flag and the variant key (a
string) for a multivariate one. The no-fragment form reproduces that exactly, so
a ref reads the way the vendor's own client would:

| Flag shape | Fragment | `Value.Bytes` |
| --- | --- | --- |
| boolean, enabled | (none) | `true` |
| boolean, disabled | (none) | `false` |
| multivariate, matched | (none) | the variant key, e.g. `control` |
| multivariate, unmatched | (none) | `false` |
| any | `#enabled` | `true` / `false`, from the `enabled` field |
| any | `#variant` | the variant key; empty for a boolean flag |
| any | `#payload` | the payload; empty when the flag has none |

A flag is multivariate exactly when PostHog sent a `variant` field, which it
sends only for a multivariate flag that matched. A disabled flag of either shape
therefore renders as `false` rather than as an empty string.

`#payload` unwraps the payload from its JSON-encoded string form: PostHog
documents `metadata.payload` as a *string* containing JSON, so returning it
verbatim would hand you a double-encoded document no JSON decode would accept. A
payload that is not a JSON string is passed through as its raw JSON instead of
being rejected.

## Sensitivity and versioning

`Value.Sensitive` is **false**. Feature flags hold rollout and configuration
state, not managed secrets, matching `providers/launchdarkly`,
`providers/flagsmith`, `providers/growthbook` and `providers/unleash`. The
`posthog` scheme is deliberately **not** in `cmd/mamori`'s secret-scheme list for
the same reason, so `mamori doctor` will not tell you to wrap a flag in
`secret.String`.

`Value.Version` is `mamori.VersionHash` of the resolved bytes.

PostHog does expose a per-flag revision, `metadata.version`, and this provider
deliberately does not use it. `metadata.version` counts edits to the flag's
*definition*, while `Value.Version` has to change whenever the *resolved bytes*
change. A flag whose evaluation flips for this distinct id without a definition
edit - a percentage rollout the id crosses, a person property changed elsewhere,
an experiment reassignment - keeps its `metadata.version`, so relying on it would
make mamori miss a real change. A content hash cannot.

Hashing the *emitted* bytes rather than the whole flag object is also deliberate:
the object carries an evaluation `reason` and `condition_index` that can change
without the value changing, and a `Version` that moved on those would cost a
spurious `PreApply` and `OnChange` on every poll.

## Authentication

PostHog's flag endpoint takes **no `Authorization` header**. The credential is
the *project API key* (`phc_...`), a public client-side token, sent in the
request body as `api_key`.

| Source | Precedence |
| --- | --- |
| `posthog.WithProjectAPIKey("phc_...")` | wins |
| `POSTHOG_PROJECT_API_KEY` | fallback, read lazily at first resolve |

The key never reaches a URL, an error message, a log line, or a mamori `Report`.
`httpcore`'s `ErrorDetail` hook is left nil, which is what guarantees no response
body - and a response body here is a set of flag payloads - can be exfiltrated
through an error string.

Point the provider at the right region with `posthog.WithHost` or `POSTHOG_HOST`.
The default is `https://us.i.posthog.com` (US Cloud); EU Cloud is
`https://eu.i.posthog.com`, and a self-hosted instance is its own domain.
**Sending evaluations to the wrong region answers as though every flag were
absent**, so naming the region explicitly is worth doing.

## Evaluation cost

Evaluation is a POST, and one evaluation returns *every* flag for the distinct
id; this provider then selects one by key. Resolving ten refs therefore performs
ten evaluations, exactly as `providers/growthbook` and `providers/flagsmith` each
re-read their whole feature set per resolve. That is deliberate: mamori's
contract is that a `Resolve` observes the backend now, and a shared cache would
make one field's poll interval silently govern another's freshness.

Because the response grows with the project rather than with the ref,
`WithMaxResponseBytes` can raise `httpcore`'s 1 MiB ceiling for a project whose
flag payloads exceed it. Single-key providers need no such knob.

## Watch

Not watchable. PostHog's flag endpoint pushes nothing, so mamori wraps this
provider in its polling adapter automatically (`WithPollInterval` plus jitter).

## Error classification

Yes, beyond `not_found`. This provider speaks HTTP through
`providers/httpcore`, so status codes are classified by
`httpcore.ClassifyStatus`:

| HTTP status | `mamori.Kind` |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

That is a real per-resolve error surface, so `providertest.Config.NoResolveErrors`
is **not** set here, unlike `providers/unleash`, `providers/split` and
`providers/configcat`, whose SDK clients return bare `bool`/`string` and
genuinely have nothing to classify.

Two conditions arrive as **HTTP 200** and are deliberately not reported as
not-found, because in both the flag's absence says nothing about whether it
exists. Calling either one not-found would have mamori quietly apply your default
in place of a live flag:

| Response body | Reported as | Why |
| --- | --- | --- |
| `"quotaLimited": ["feature_flags"]` | `rate_limited` | The project is over its billing quota and PostHog has paused flag evaluation, answering `200` with an empty `flags` object. |
| `"errorsWhileComputingFlags": true`, flag absent | `unavailable` | PostHog could not compute some flags for this request; the flag may exist and simply not have been computed. |

A `quotaLimited` array naming some *other* resource (say `recordings`) says
nothing about flags and is ignored.

A flag PostHog returned nothing for, with neither condition set, is
`mamori.ErrNotFound`.

## The vendor contract this provider targets

Pinned from PostHog's live documentation. **Which endpoint version:** `/flags`
with `?v=2`, the current documented evaluation endpoint, which supersedes the
older `/decide`. The `v=2` parameter selects the response envelope whose per-flag
objects carry `enabled`, `variant` and `metadata`; without it PostHog answers
with the older flat `featureFlags` map, which cannot distinguish a disabled flag
from an absent one.

Request, quoted from the documentation:

```shell
curl -v -L --header "Content-Type: application/json" -d '{
    "api_key": "<ph_project_token>",
    "distinct_id": "distinct_id_of_your_user",
    "groups" : {
        "group_type": "group_id"
    }
}' "<ph_client_api_host>/flags?v=2"
```

Optional body fields, also documented: `person_properties` and
`group_properties`. This provider sends `groups`, `person_properties` and
`group_properties` only when configured, via `WithGroups`,
`WithPersonProperties` and `WithGroupProperties`.

Success response for a **boolean** flag:

```json
{
  "flags": {
    "my-awesome-flag": {
      "key": "my-awesome-flag",
      "enabled": true,
      "reason": {
        "code": "condition_match",
        "condition_index": 0,
        "description": "Condition set 1 matched"
      },
      "metadata": {
        "id": 1,
        "version": 1,
        "payload": "{\"example\": \"json\", \"payload\": \"value\"}"
      }
    }
  },
  "errorsWhileComputingFlags": false,
  "requestId": "550e8400-e29b-41d4-a716-446655440000"
}
```

Success response for a **multivariate** flag - the only structural difference is
the added `variant` field:

```json
{
  "flags": {
    "my-multivariate-flag" :{
      "key":"my-multivariate-flag",
      "enabled": true,
      "variant": "some-string-value",
      "reason": {
        "code": "condition_match",
        "condition_index": 1,
        "description": "Condition set 2 matched"
      },
      "metadata": {
        "id": 2,
        "version": 42
      }
    }
  }
}
```

### Documented versus live-verified

**Nobody on this project has PostHog credentials, so every row below is taken
from the vendor's documentation and has not been confirmed against a live
project.** The `//go:build integration` tests in this module are the mechanism
for confirming them; they skip cleanly until someone supplies
`MAMORI_POSTHOG_PROJECT_API_KEY` and `MAMORI_POSTHOG_FLAG`.

| Behaviour | State |
| --- | --- |
| `POST /flags?v=2`, body field `api_key`, `distinct_id` | documented |
| `groups`, `person_properties`, `group_properties` body fields | documented |
| Boolean flag carries `enabled` and no `variant` | documented |
| Multivariate flag adds `variant` | documented |
| `metadata.payload` is a JSON-encoded string | documented |
| An absent flag is simply missing from `flags` | documented (inferred from the response shape; the docs do not state it in words) |
| `quotaLimited: ["feature_flags"]` on a billing pause | documented |
| `errorsWhileComputingFlags` | documented (the field is shown in every example; its exact trigger is not spelled out) |
| HTTP status codes for failures | **not documented** - see below |
| Every mapping in this module's tests | verified against an in-process fake, not a live project |

### Two honest gaps in the vendor documentation

**PostHog does not enumerate failure status codes for `/flags`.** The API
overview instead says that "Public POST-only endpoints such as event capture
(`/e`, `/i/v0/e`) and feature flag evaluation (`/flags`) have no request-level
rate limits", and that over-quota projects get a `200` naming the limited
resources in the body rather than a failure status. This provider therefore does
not hand-roll a PostHog-specific status table: it inherits
`httpcore.ClassifyStatus`, the same mapping every other HTTP-backed mamori
provider uses, and treats the two documented `200`-with-a-problem bodies as the
real per-resolve failure surface. If PostHog later documents specific codes, the
inherited table already covers 400/401/403/404/408/429 correctly.

**Two documentation pages disagree about the credential's field name.** The
`/flags` API reference and every code sample in the feature-flags integration
snippet use `api_key`; the older feature-flags installation page shows `token`.
This provider sends **`api_key`**, because that is what the endpoint's own
reference page and all four language samples (curl, Python, Node, PHP) use, and
the `token` spelling appears only in the narrower installation page. This is
flagged rather than resolved silently, since it is the one field whose being
wrong would make every evaluation fail identically to a missing project.

### Documentation URLs

- <https://posthog.com/docs/api/flags> - the flags evaluation endpoint reference
- <https://posthog.com/docs/api/flags.md> - the same page in markdown, which carries the full request and response examples
- <https://github.com/PostHog/posthog.com/blob/master/contents/docs/integrate/feature-flags-code/_snippets/feature-flags-code-api.mdx> - the source of the quoted curl/Python/Node samples and both response shapes
- <https://posthog.com/docs/feature-flags/installation/api> - API feature-flag installation (the page showing `token`)
- <https://posthog.com/docs/api> - API overview, the source of the rate-limit and quota statements
- <https://posthog.com/docs/libraries/go> - the PostHog Go SDK, whose `getFeatureFlag` return convention (variant string for multivariate, boolean otherwise) the no-fragment mapping reproduces

The PostHog Go SDK is cited for its documented behaviour only. This module
depends on the standard library, `github.com/xavidop/mamori`, and
`github.com/xavidop/mamori/providers/httpcore`, and nothing else.

## Configuration

```go
import (
    "os"

    "github.com/xavidop/mamori"
    posthog "github.com/xavidop/mamori/providers/posthog"
)

mamori.WithProvider(posthog.New(
    posthog.WithProjectAPIKey(os.Getenv("POSTHOG_PROJECT_API_KEY")),
    posthog.WithHost("https://eu.i.posthog.com"),
    posthog.WithDistinctID("svc-billing"),
    posthog.WithGroups(map[string]string{"company": "acme"}),
    posthog.WithPersonProperties(map[string]any{"plan": "enterprise"}),
))
```

| Option | Environment variable | Default |
| --- | --- | --- |
| `WithProjectAPIKey` | `POSTHOG_PROJECT_API_KEY` | none; a resolve without one fails with `mamori.ErrInvalid` |
| `WithHost` | `POSTHOG_HOST` | `https://us.i.posthog.com` |
| `WithDistinctID` | `POSTHOG_DISTINCT_ID` | `mamori` |
| `WithGroups` | - | none |
| `WithPersonProperties` | - | none |
| `WithGroupProperties` | - | none |
| `WithHTTPClient` | - | `httpcore`'s 30s-timeout client |
| `WithMaxResponseBytes` | - | `httpcore.DefaultMaxBody` (1 MiB) |

`New` never contacts PostHog and never fails, so the blank import registers a
provider that configures itself from the environment at first resolve.

## Development

This package is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/posthog
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet -tags integration ./...
```

The conformance kit runs against an in-process `http.RoundTripper` rather than an
`httptest.Server`: the kit's `NoGoroutineLeak` case runs `goleak.VerifyNone`, and
a live server's accept goroutine does not survive it. The fake checks
`req.Context().Err()` itself, because `net/http` enforces the request context in
the Transport rather than in `Client.Do`, and without that check the
`ContextCancel` case would pass vacuously.

Live tests need a real project and skip without one:

```sh
export MAMORI_POSTHOG_PROJECT_API_KEY=phc_...
export MAMORI_POSTHOG_FLAG=new-checkout
GOWORK=off go test -tags integration -run Integration ./...
```

They log a byte count and the facet name, never a key or a resolved value.
