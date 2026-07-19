---
layout: ../../../layouts/DocsLayout.astro
title: Firebase Remote Config provider
---

# Firebase Remote Config

Read a parameter from your Firebase Remote Config server template - the classic dynamic-config use case.

| | |
| --- | --- |
| Scheme | `firebase-rc://` |
| Module | `github.com/xavidop/mamori/providers/firebase-rc` |
| Sensitive | no |
| Watch | poll |
| Auth | Application Default Credentials (`WithProjectID`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/firebase-rc
```

```go
import _ "github.com/xavidop/mamori/providers/firebase-rc"
```

## Using the ref

A `firebase-rc://` ref points at one parameter in your project's server Remote Config template.

```text
firebase-rc://<parameter-key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<parameter-key>` | yes | The name of a parameter in the server Remote Config template. Its server-side default value becomes the value. |
| `#json-key` | no | Treat the parameter value as a JSON object and return one field of it. |

**Examples**

- `firebase-rc://welcome_banner` returns the server-side value of the `welcome_banner` parameter.
- `firebase-rc://max_items` returns the `max_items` parameter - pair it with an `int` field.
- `firebase-rc://feature_flags#new_ui` treats the `feature_flags` parameter as JSON and returns its `new_ui` field.

```go
type Config struct {
	Banner   string `source:"firebase-rc://welcome_banner"`
	MaxItems int    `source:"firebase-rc://max_items"`
}
```

The provider reads the current server template (the one used by the Admin SDK and server workloads) and returns the named parameter's default value. A missing parameter, or one configured to use the in-app default (no server value), resolves to not-found, so `default:` / `optional:"true"` applies. `Value.Version` is the template's version number, which is template-wide - it changes whenever any parameter is published, so mamori may occasionally re-apply an unchanged value (harmless).

## Watch

The server template has no push channel, so mamori polls (`WithPollInterval` + jitter).

## Configuration

```go
import rcprov "github.com/xavidop/mamori/providers/firebase-rc"

mamori.WithProvider(rcprov.New(rcprov.WithProjectID("my-project")))
```

Authentication uses Application Default Credentials (a service account via `GOOGLE_APPLICATION_CREDENTIALS`, or workload identity). Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
