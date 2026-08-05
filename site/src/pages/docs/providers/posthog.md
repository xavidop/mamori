---
layout: ../../../layouts/DocsLayout.astro
title: PostHog provider
---

# PostHog

Evaluate a [PostHog](https://posthog.com) feature flag and use the result as config. Pure `net/http` on top of [`httpcore`](/docs/writing-a-provider/httpcore/), no third-party SDK.

| | |
| --- | --- |
| Scheme | `posthog://` |
| Module | `github.com/xavidop/mamori/providers/posthog` |
| Sensitive | no |
| Watch | poll |
| Auth | `POSTHOG_PROJECT_API_KEY`, sent in the request body rather than a header |

## Install

```bash
go get github.com/xavidop/mamori/providers/posthog
```

```go
import _ "github.com/xavidop/mamori/providers/posthog"
```

## Using the ref

A `posthog://` ref points at one feature flag. Without a fragment it resolves the way PostHog's own SDKs do; a fragment names a specific facet of the evaluated result.

```text
posthog://<flag-key>[#enabled | #variant | #payload]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<flag-key>` | yes | The feature flag's key in the PostHog project. |
| `#enabled` | no | The flag's enabled state, as `true` / `false`. |
| `#variant` | no | A multivariate flag's variant key. Empty for a boolean flag. |
| `#payload` | no | The flag's payload. Empty when the flag has none. |

**Examples**

- `posthog://new-checkout` resolves to `true` / `false` - pair it with a `bool` field.
- `posthog://pricing-test` resolves to the assigned variant key, e.g. `control`.
- `posthog://pricing-test#variant` does the same, explicitly.
- `posthog://pricing-test#payload` resolves to that flag's payload (often JSON, which you can `flatten:"json"`).

```go
type Config struct {
	NewCheckout bool   `source:"posthog://new-checkout"`
	Pricing     string `source:"posthog://pricing-test" default:"control"`
	Copy        string `source:"posthog://pricing-test#payload"`
}
```

Any fragment other than those three is rejected with `mamori.ErrInvalid` before a request is sent, so a typo surfaces at `mamori doctor` time rather than resolving to an empty string in production.

A distinct id never appears in a ref: PostHog evaluates flags *for* a distinct id, which identifies the evaluation context rather than the flag, so it is provider-level configuration (`WithDistinctID`, below). **Use a stable id, not a memorable one.** A percentage rollout hashes the distinct id, so a fresh random id per process would put two replicas of the same service on opposite sides of the same 50% rollout.

## Value mapping

PostHog's own SDKs return a boolean for a boolean flag and the variant key for a multivariate one. The no-fragment form reproduces that exactly.

| Flag shape | Fragment | Resolved bytes |
| --- | --- | --- |
| boolean, enabled | (none) | `true` |
| boolean, disabled | (none) | `false` |
| multivariate, matched | (none) | the variant key, e.g. `control` |
| multivariate, unmatched | (none) | `false` |
| any | `#enabled` | `true` / `false` |
| any | `#variant` | the variant key; empty for a boolean flag |
| any | `#payload` | the payload; empty when the flag has none |

A flag counts as multivariate exactly when PostHog sent a `variant` field, which it does only for a multivariate flag that matched. **A disabled flag of either shape therefore resolves to `false`, never to an empty string.** That distinction is why the provider calls `POST /flags?v=2`: the `v=2` envelope carries a per-flag `enabled`, `variant` and `metadata`, while the older flat `featureFlags` map cannot tell a disabled flag from one that does not exist. `#payload` unwraps the payload from its JSON-encoded string form, since PostHog documents `metadata.payload` as a string containing JSON and returning it verbatim would hand you a double-encoded document.

## Error classification

Failures are classified through `httpcore`'s shared status table:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

Two conditions arrive as **HTTP 200** with the flag simply missing from the response, and neither is reported as not-found: in both, the absence says nothing about whether the flag exists, and calling either one not-found would have mamori quietly apply your `default:` in place of a live flag.

| Response body | Reported as |
| --- | --- |
| `"quotaLimited": ["feature_flags"]`, the project is over its billing quota and evaluation is paused | `rate_limited` |
| `"errorsWhileComputingFlags": true` with the flag absent | `unavailable` |

A `quotaLimited` array naming some other resource says nothing about flags and is ignored. A flag PostHog computed successfully and did not return is a genuine `not_found`, so a field's `default:` applies as usual.

## Watch

PostHog's flag endpoint pushes nothing, so mamori polls (`WithPollInterval` + jitter). Each poll is a full evaluation, and one evaluation returns every flag for the distinct id; the provider then selects one by key, so ten refs cost ten evaluations. `Value.Version` is a content hash of the resolved bytes rather than PostHog's per-flag `metadata.version`, which counts edits to the flag's definition. A flag whose evaluation flips for this distinct id without a definition edit - a percentage rollout the id crosses, an experiment reassignment - is a real change, and hashing the bytes is what catches it.

## Configuration

```go
import posthogprov "github.com/xavidop/mamori/providers/posthog"

mamori.WithProvider(posthogprov.New(
	posthogprov.WithProjectAPIKey(os.Getenv("POSTHOG_PROJECT_API_KEY")),
	posthogprov.WithHost("https://eu.i.posthog.com"),
	posthogprov.WithDistinctID("svc-billing"),
	posthogprov.WithGroups(map[string]string{"company": "acme"}),
))
```

| Option | Environment variable | Default |
| --- | --- | --- |
| `WithProjectAPIKey` | `POSTHOG_PROJECT_API_KEY` | none; a resolve without one fails with `mamori.ErrInvalid` |
| `WithHost` | `POSTHOG_HOST` | `https://us.i.posthog.com` |
| `WithDistinctID` | `POSTHOG_DISTINCT_ID` | `mamori` |
| `WithGroups` / `WithPersonProperties` / `WithGroupProperties` | - | none |
| `WithHTTPClient` | - | 30s-timeout client |
| `WithMaxResponseBytes` | - | 1 MiB; raise it for a project whose flag payloads exceed it |

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting PostHog. Without `WithHTTPClient` it releases nothing: this provider builds no default client of its own to hold onto. A client injected with `WithHTTPClient` is never closed: `Close` may return its idle connections to the pool, but leaves the client usable.

The credential is the *project API key* (`phc_...`), a public client-side token. PostHog's flag endpoint takes no `Authorization` header: the key travels in the request body as `api_key`, and never reaches a URL, an error, a log line, or a `Report`.

Point the provider at the right region. The default is US Cloud; EU Cloud is `https://eu.i.posthog.com`, and a self-hosted instance is its own domain. **Sending evaluations to the wrong region answers as though every flag were absent**, so every field takes its `default:` and nothing fails.

Verified against an in-process HTTP fake, so the conformance kit runs without a PostHog project. Nobody on this project has credentials, so the request and response shapes follow the vendor's documentation rather than a live capture, and the `//go:build integration` tests confirm them once `MAMORI_POSTHOG_PROJECT_API_KEY` and `MAMORI_POSTHOG_FLAG` are set.
