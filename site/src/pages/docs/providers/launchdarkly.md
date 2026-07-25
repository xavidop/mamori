---
layout: ../../../layouts/DocsLayout.astro
title: LaunchDarkly provider
---

# LaunchDarkly

Load a LaunchDarkly feature flag's value as config, with **native watch** via the LaunchDarkly streaming SDK.

| | |
| --- | --- |
| Scheme | `launchdarkly://` |
| Module | `github.com/xavidop/mamori/providers/launchdarkly` |
| Sensitive | no |
| Watch | native (streaming) |
| Auth | `LAUNCHDARKLY_SDK_KEY` |

## Install

```bash
go get github.com/xavidop/mamori/providers/launchdarkly
```

```go
import _ "github.com/xavidop/mamori/providers/launchdarkly"
```

## Using the ref

A `launchdarkly://` ref points at one flag key. The flag is evaluated for a fixed evaluation context (your service), and its variation becomes the value.

```text
launchdarkly://<flag-key>
```

| Part | Required | What it means |
| --- | --- | --- |
| `<flag-key>` | yes | The LaunchDarkly flag key. |

The variation maps to bytes by type: a boolean flag resolves to `true` / `false`, a string flag to its string, a number to its decimal form, and a JSON flag to its JSON encoding. A flag key that does not exist resolves to not-found, so `default:` / `optional:"true"` applies.

**Examples**

- `launchdarkly://enable-new-checkout` on a boolean flag resolves to `true` or `false` - pair it with a `bool` field.
- `launchdarkly://max-batch-size` on a number flag resolves to e.g. `250` - pair it with an `int` field and a `validate` rule.
- `launchdarkly://pricing-config` on a JSON flag resolves to the JSON, which you can decode with `flatten:"json"`.

```go
type Config struct {
	NewCheckout bool          `source:"launchdarkly://enable-new-checkout"`
	Pricing     PricingConfig `source:"launchdarkly://pricing-config" flatten:"json"`
}
```

The evaluation context defaults to a stable key (`mamori`); override it with `WithContextKey`.

## Error classification

Beyond the not-found case, other evaluation failures are classified by the SDK's per-evaluation error reason:

| Evaluation error kind | mamori kind |
| --- | --- |
| `CLIENT_NOT_READY` | `unavailable` |
| `FLAG_NOT_FOUND` | not classified here - already drives not-found |
| anything else (`MALFORMED_FLAG`, `USER_NOT_SPECIFIED`, `WRONG_TYPE`, `EXCEPTION`, ...) | `unknown` |

LaunchDarkly's per-evaluation error vocabulary is thin: `CLIENT_NOT_READY` (evaluating before the client finished connecting) is the only kind with a confirmed, unambiguous meaning, so it is the only one mapped. The rest describe internal or data conditions with no reliable single real-world cause and are deliberately left `unknown` rather than guessed at.

## Watch

`Watch` uses the SDK flag tracker: LaunchDarkly streams flag changes, and mamori emits an update the instant the flag's value for your context changes. This is a genuine push, not polling.

## Configuration

```go
import ldprov "github.com/xavidop/mamori/providers/launchdarkly"

mamori.WithProvider(ldprov.New(ldprov.WithSDKKey(os.Getenv("LAUNCHDARKLY_SDK_KEY"))))
```

Verified with an injected fake evaluator (including value-change subscriptions, not-found, and injected per-flag failures for error classification), so the watch and `providertest` `ErrorClassification` conformance checks run without a live LaunchDarkly. A real-SDK test is provided behind `//go:build integration`.
