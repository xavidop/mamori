# mamori AWS provider

AWS Secrets Manager, SSM Parameter Store, and AWS AppConfig providers for [mamori](https://github.com/xavidop/mamori), built on `aws-sdk-go-v2`.

```bash
go get github.com/xavidop/mamori/providers/aws
```

```go
import _ "github.com/xavidop/mamori/providers/aws" // registers aws-sm://, aws-ps://, and aws-appconfig://
```

## Schemes

| Scheme | Backend | Sensitive | Watch |
|---|---|---|---|
| `aws-sm://<secret-id>[#json-key]` | Secrets Manager | ✅ | poll |
| `aws-ps://<parameter-name>[#json-key]` | SSM Parameter Store | only `SecureString` | poll |
| `aws-appconfig://<app>/<env>/<profile>[#json-key]` | AWS AppConfig | no | poll |

```go
type Config struct {
    DBPassword secret.String `source:"aws-sm://prod/db#password"`  // one key of a JSON secret
    APIKey     secret.String `source:"aws-sm://prod/api-key"`      // whole secret string
    LogLevel   string        `source:"aws-ps:///myapp/log-level"`  // Parameter Store
    Flags      string        `source:"aws-appconfig://myapp/prod/flags"` // AppConfig
}
```

- `#json-key` selects a field from a JSON secret/configuration payload (via `mamori.SelectKey`).
- Secrets Manager sets `Value.Version` from the `VersionId`; Parameter Store from the parameter `Version`; AppConfig from the configuration profile's `VersionLabel` when the source is an AppConfig-hosted configuration, falling back to `mamori.VersionHash` for every other configuration source (Parameter Store, SSM documents, Secrets Manager, S3, or Feature Flags), which have no such label.
- Secrets Manager implements `BatchProvider` (`BatchGetSecretValue`) so multiple secrets resolve in one API call.

## `aws-appconfig://` ref grammar

```text
aws-appconfig://<application>/<environment>/<profile>[#json-key][?minPoll=<seconds>]
```

All three path segments are required and may each be either the resource's AWS-assigned ID or its name; the provider passes them through verbatim and lets AppConfig Data resolve them:

- `<application>` - the AppConfig application ID or name.
- `<environment>` - the AppConfig environment ID or name.
- `<profile>` - the configuration profile ID or name.
- `#json-key` - optional; selects one field from a JSON configuration payload (via `mamori.SelectKey`), identically to the other two schemes.
- `?minPoll=<seconds>` - optional; sets `RequiredMinimumPollIntervalInSeconds` on the session. It has no observable effect today: the floor constrains a session's second and later calls, and every session here is discarded after its first. It is accepted because it is the correct plumbing for the field and would become meaningful if a resident session ever existed.

`Resolve` costs two AWS API calls (`StartConfigurationSession` then `GetLatestConfiguration`), not one, because AppConfig Data is a session protocol: a session that already holds the current version receives an *empty* payload from `GetLatestConfiguration`, so a provider that opened one session and reused it across calls would return the configuration once and empty bytes on every call after. `Resolve` therefore starts a fresh session and discards it on every call, paying the extra request to keep every call stateless and correct.

AppConfig values are **not** marked `Sensitive`: AppConfig is a configuration service, not a secret store, so nothing about its payloads warrants secret-hygiene treatment by default. Store secrets in Secrets Manager or Parameter Store `SecureString` and reference them from your AppConfig-managed configuration instead.

## Authentication

Uses the standard AWS credential chain (env vars, shared config, IAM role, etc.). Set the region with `AWS_REGION` or explicitly:

```go
mamori.WithProvider(aws.NewSecretsManager(aws.WithRegion("eu-west-1")))
mamori.WithProvider(aws.NewParameterStore(aws.WithRegion("eu-west-1")))
mamori.WithProvider(aws.NewAppConfig(aws.WithRegion("eu-west-1")))
```

## Watch

None of the three backends has native change notification, so mamori polls (interval + jitter, `Value.Version` comparison). Configure with `mamori.WithPollInterval`.

## Error classification

Failures are classified so `mamori.ErrorKind` can distinguish them:

| AWS error code | mamori kind |
|---|---|
| `ResourceNotFoundException`, `ParameterNotFound`, `ParameterVersionNotFound` | `not_found` |
| `AccessDeniedException` | `permission_denied` |
| `UnrecognizedClientException`, `ExpiredTokenException`, `InvalidSignatureException`, `MissingAuthenticationToken`, `IncompleteSignature` | `unauthenticated` |
| `ThrottlingException`, `Throttling`, `TooManyRequestsException`, `RequestLimitExceeded` | `rate_limited` |
| `InternalServiceError`, `InternalServerError`, `InternalFailure`, `InternalServerException`, `ServiceUnavailable`, `ServiceUnavailableException` | `unavailable` |
| `InvalidParameterException`, `InvalidRequestException`, `ValidationException`, `InvalidParameterValue`, `InvalidKeyId`, `BadRequestException` | `invalid` |
| anything else | `unknown` |

`InternalServerException` and `BadRequestException` are AppConfig Data's error
codes: AppConfig spells its server error differently from the
`InternalServerError` the other two backends use, and `BadRequestException` is
what a reused or expired configuration session token comes back as.

Codes not listed above (including any not yet added to this table) report
`unknown` rather than being guessed at. Notably, Secrets Manager's
`DecryptionFailure` is deliberately left unmapped: it can mean a KMS key
policy problem, a disabled key, or a KMS outage, and does not map cleanly to
one kind, so reporting it as `unknown` is the honest outcome.

The original SDK error remains reachable with `errors.As`, so existing code
matching on `*smtypes.ResourceNotFoundException` keeps working.

## What is verified

- ✅ Unit tests against injected fake SDK clients, and the [`providertest`](../../providertest) conformance kit against in-memory fakes for all three schemes.
- ⚠️ Live AWS behavior is exercised by `//go:build integration` tests that require real credentials and are **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
