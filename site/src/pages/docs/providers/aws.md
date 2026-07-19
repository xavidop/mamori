---
layout: ../../../layouts/DocsLayout.astro
title: AWS provider
---

# AWS

Secrets Manager and SSM Parameter Store, built on `aws-sdk-go-v2`.

| | |
| --- | --- |
| Schemes | `aws-sm://` `aws-ps://` |
| Module | `github.com/xavidop/mamori/providers/aws` |
| Sensitive | Secrets Manager: yes · Parameter Store: SecureString only |
| Watch | poll |
| Auth | default AWS credential chain (`AWS_REGION`, env, shared config, IAM role) |

## Install

```bash
go get github.com/xavidop/mamori/providers/aws
```

```go
import _ "github.com/xavidop/mamori/providers/aws" // registers aws-sm:// and aws-ps://
```

## Using the ref

An `aws-sm://` ref points at one secret in AWS Secrets Manager; an `aws-ps://` ref points at one parameter in SSM Parameter Store.

```text
aws-sm://<secret-id>[#json-key]
aws-ps://<parameter-name>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secret-id>` | yes | The Secrets Manager secret name or ARN. |
| `<parameter-name>` | yes | The Parameter Store name, including its leading slash, e.g. `/myapp/log-level`. |
| `#json-key` | no | Select one field from a JSON secret/parameter payload (via `mamori.SelectKey`). |

**Examples**

- `aws-sm://prod/api-key` returns the whole secret string - use it for an opaque token.
- `aws-sm://prod/db#password` returns just the `password` field of a JSON secret.
- `aws-ps:///myapp/log-level` reads the `/myapp/log-level` parameter (note the extra slash: the `aws-ps://` scheme plus the `/myapp/...` name).
- `aws-ps:///myapp/db#password` selects `password` from a JSON parameter.

```go
type Config struct {
	APIKey     secret.String `source:"aws-sm://prod/api-key"`      // whole secret string
	DBPassword secret.String `source:"aws-sm://prod/db#password"`  // one key of a JSON secret
	LogLevel   string        `source:"aws-ps:///myapp/log-level"`  // SecureString is marked sensitive
}
```

Secrets Manager values are always `Sensitive`; Parameter Store reads with `WithDecryption=true` and marks only `SecureString` parameters `Sensitive`. `Value.Version` is the secret's `VersionId` or the parameter's numeric `Version`. Secrets Manager implements `BatchProvider`, so multiple `aws-sm://` refs resolve in one `BatchGetSecretValue` call.

## Explicit configuration

```go
import awsprov "github.com/xavidop/mamori/providers/aws"

mamori.WithProvider(awsprov.NewSecretsManager(awsprov.WithRegion("eu-west-1")))
mamori.WithProvider(awsprov.NewParameterStore(awsprov.WithRegion("eu-west-1")))
```

## Watch

Neither backend has native change notification, so mamori polls (`WithPollInterval` + jitter, `Value.Version` comparison). For push-based rotation you can pair this with an EventBridge -> SQS trigger in your app and call `Load` on demand.

Verified by unit tests and the `providertest` conformance kit against in-memory fakes; live AWS behavior is covered by `//go:build integration` tests.
