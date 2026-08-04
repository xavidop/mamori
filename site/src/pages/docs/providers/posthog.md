---
layout: ../../../layouts/DocsLayout.astro
title: PostHog provider
---

# PostHog

Evaluate a [PostHog](https://posthog.com) feature flag and use the result as config.

| | |
| --- | --- |
| Scheme | `posthog://` |
| Module | `github.com/xavidop/mamori/providers/posthog` |
| Sensitive | no |
| Watch | poll |
| Auth | `POSTHOG_PROJECT_API_KEY` (sent in the request body, not a header) |

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

Any other fragment is rejected with `mamori.ErrInvalid` rather than resolving to an empty string, so a typo surfaces at `mamori doctor` time instead of in production. A flag PostHog returned nothing for resolves to not-found.

**Examples**

- `posthog://new-checkout` resolves to `true` / `false` - pair it with a `bool` field.
- `posthog://pricing-test` resolves to the assigned variant key, e.g. `control`.
- `posthog://pricing-test#variant` does the same, explicitly.
- `posthog://pricing-test#payload` resolves to that flag's payload (often JSON, which you can `flatten:"json"`).

## Where the distinct id goes

Nowhere in the ref. PostHog evaluates flags **for a distinct id**, and a distinct id identifies the evaluation *context*, not the flag, so it is provider-level configuration - the same choice `providers/launchdarkly` makes for its evaluation context, with the same `mamori` default.

```go
mamori.WithProvider(posthogprov.New(
	posthogprov.WithProjectAPIKey(os.Getenv("POSTHOG_PROJECT_API_KEY")),
	posthogprov.WithDistinctID("svc-billing"),
))
```

A **stable** id matters more than a memorable one: a percentage rollout hashes the distinct id, so a random id per process would put two replicas of the same service on opposite sides of the same 50% rollout.

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

A flag is multivariate exactly when PostHog sent a `variant` field, which it sends only for a multivariate flag that matched. A disabled flag of either shape therefore renders as `false` rather than as an empty string.

`#payload` unwraps the payload from its JSON-encoded string form, because PostHog documents `metadata.payload` as a *string* containing JSON and returning it verbatim would hand you a double-encoded document.

## Versioning

`Value.Version` is a content hash of the resolved bytes.

PostHog exposes a per-flag revision, `metadata.version`, which this provider deliberately does not use: it counts edits to the flag's *definition*, while `Value.Version` has to change whenever the *resolved bytes* change. A flag whose evaluation flips for this distinct id without a definition edit - a percentage rollout the id crosses, an experiment reassignment - keeps its `metadata.version`, so relying on it would make mamori miss a real change.

## Watch

PostHog's flag endpoint pushes nothing, so mamori polls (`WithPollInterval` + jitter).

Each poll is a full evaluation, and one evaluation returns every flag for the distinct id; the provider then selects one by key. Ten refs means ten evaluations, the same as `growthbook` and `flagsmith`. `WithMaxResponseBytes` raises the 1 MiB response ceiling for a project whose flag payloads exceed it.

## Error classification

Yes, beyond not-found. This provider speaks HTTP through `httpcore`, so status codes map through `httpcore.ClassifyStatus`: 400/422 to `invalid`, 401 to `unauthenticated`, 403 to `permission_denied`, 404 to `not_found`, 408/429 to `rate_limited`, anything else to `unavailable`. `providertest.Config.NoResolveErrors` is not set.

Two conditions arrive as **HTTP 200** and are deliberately not reported as not-found, because in both the flag's absence says nothing about whether it exists - calling either one not-found would have mamori quietly apply your default in place of a live flag:

| Response body | Reported as |
| --- | --- |
| `"quotaLimited": ["feature_flags"]` (the project is over its billing quota and evaluation is paused) | `rate_limited` |
| `"errorsWhileComputingFlags": true` with the flag absent | `unavailable` |

A `quotaLimited` array naming some other resource says nothing about flags and is ignored.

## Configuration

```go
import posthogprov "github.com/xavidop/mamori/providers/posthog"
```

```go
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
| `WithMaxResponseBytes` | - | 1 MiB |

The credential is the *project API key* (`phc_...`), a public client-side token. PostHog's flag endpoint takes no `Authorization` header; the key travels in the request body as `api_key`, and never reaches a URL, an error, a log line, or a `Report`.

Point the provider at the right region. The default is US Cloud; EU Cloud is `https://eu.i.posthog.com`, and a self-hosted instance is its own domain. **Sending evaluations to the wrong region answers as though every flag were absent.**

## Which endpoint this targets

`POST /flags?v=2`, the current documented evaluation endpoint, which supersedes the older `/decide`. The `v=2` parameter selects the envelope whose per-flag objects carry `enabled`, `variant` and `metadata`; without it PostHog answers with the older flat `featureFlags` map, which cannot distinguish a disabled flag from an absent one.

Two gaps in the vendor documentation are worth knowing, and the module README records both in full:

- **PostHog does not enumerate failure status codes for `/flags`.** Its API overview instead states that public POST-only endpoints including `/flags` have no request-level rate limits, and that over-quota projects get a `200` naming the limited resources in the body. This provider therefore inherits `httpcore`'s shared status table rather than hand-rolling a vendor-specific one.
- **Two documentation pages disagree about the credential's field name**, `api_key` versus `token`. This provider sends `api_key`, which is what the endpoint's own reference page and all its language samples use.

Nobody on this project has PostHog credentials, so the request and response shapes here are taken from the vendor's documentation and verified against an in-process fake, **not** against a live project. The module's `//go:build integration` tests are the mechanism for confirming them and skip cleanly until someone supplies `MAMORI_POSTHOG_PROJECT_API_KEY` and `MAMORI_POSTHOG_FLAG`. See the [module README](https://github.com/xavidop/mamori/tree/main/providers/posthog) for the quoted request and response bodies and the documentation URLs.
