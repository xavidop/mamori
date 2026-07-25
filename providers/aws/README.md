# mamori AWS provider

AWS Secrets Manager and SSM Parameter Store providers for [mamori](https://github.com/xavidop/mamori), built on `aws-sdk-go-v2`.

```bash
go get github.com/xavidop/mamori/providers/aws
```

```go
import _ "github.com/xavidop/mamori/providers/aws" // registers aws-sm:// and aws-ps://
```

## Schemes

| Scheme | Backend | Sensitive | Watch |
|---|---|---|---|
| `aws-sm://<secret-id>[#json-key]` | Secrets Manager | ✅ | poll |
| `aws-ps://<parameter-name>[#json-key]` | SSM Parameter Store | only `SecureString` | poll |

```go
type Config struct {
    DBPassword secret.String `source:"aws-sm://prod/db#password"`  // one key of a JSON secret
    APIKey     secret.String `source:"aws-sm://prod/api-key"`      // whole secret string
    LogLevel   string        `source:"aws-ps:///myapp/log-level"`  // Parameter Store
}
```

- `#json-key` selects a field from a JSON secret payload (via `mamori.SelectKey`).
- Secrets Manager sets `Value.Version` from the `VersionId`; Parameter Store from the parameter `Version`.
- Secrets Manager implements `BatchProvider` (`BatchGetSecretValue`) so multiple secrets resolve in one API call.

## Authentication

Uses the standard AWS credential chain (env vars, shared config, IAM role, etc.). Set the region with `AWS_REGION` or explicitly:

```go
mamori.WithProvider(aws.NewSecretsManager(aws.WithRegion("eu-west-1")))
mamori.WithProvider(aws.NewParameterStore(aws.WithRegion("eu-west-1")))
```

## Watch

Neither backend has native change notification, so mamori polls (interval + jitter, `Value.Version` comparison). Configure with `mamori.WithPollInterval`.

## Error classification

Failures are classified so `mamori.ErrorKind` can distinguish them:

| AWS error code | mamori kind |
|---|---|
| `ResourceNotFoundException`, `ParameterNotFound`, `ParameterVersionNotFound` | `not_found` |
| `AccessDeniedException` | `permission_denied` |
| `UnrecognizedClientException`, `ExpiredTokenException`, `InvalidSignatureException`, `MissingAuthenticationToken`, `IncompleteSignature` | `unauthenticated` |
| `ThrottlingException`, `Throttling`, `TooManyRequestsException`, `RequestLimitExceeded` | `rate_limited` |
| `InternalServiceError`, `InternalServerError`, `InternalFailure`, `ServiceUnavailable`, `ServiceUnavailableException` | `unavailable` |
| `InvalidParameterException`, `InvalidRequestException`, `ValidationException`, `InvalidParameterValue`, `InvalidKeyId` | `invalid` |
| anything else | `unknown` |

Codes not listed above (including any not yet added to this table) report
`unknown` rather than being guessed at. Notably, Secrets Manager's
`DecryptionFailure` is deliberately left unmapped: it can mean a KMS key
policy problem, a disabled key, or a KMS outage, and does not map cleanly to
one kind, so reporting it as `unknown` is the honest outcome.

The original SDK error remains reachable with `errors.As`, so existing code
matching on `*smtypes.ResourceNotFoundException` keeps working.

## What is verified

- ✅ Unit tests against injected fake SDK clients, and the [`providertest`](../../providertest) conformance kit against in-memory fakes for both schemes.
- ⚠️ Live AWS behavior is exercised by `//go:build integration` tests that require real credentials and are **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
