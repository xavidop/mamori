---
layout: ../../../layouts/DocsLayout.astro
title: AWS AppConfig provider
---

# AWS AppConfig

AWS AppConfig, built on `aws-sdk-go-v2`'s `appconfigdata` client. Ships in the same module as [AWS Secrets Manager and SSM Parameter Store](/docs/providers/aws).

| | |
| --- | --- |
| Schemes | `aws-appconfig://` |
| Module | `github.com/xavidop/mamori/providers/aws` |
| Sensitive | no |
| Watch | poll |
| Auth | default AWS credential chain (`AWS_REGION`, env, shared config, IAM role) |

## Install

```bash
go get github.com/xavidop/mamori/providers/aws
```

```go
import _ "github.com/xavidop/mamori/providers/aws" // registers aws-sm://, aws-ps://, and aws-appconfig://
```

## Using the ref

An `aws-appconfig://` ref points at one configuration profile in one AWS AppConfig environment.

```text
aws-appconfig://<application>/<environment>/<profile>[#json-key][?minPoll=<seconds>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<application>` | yes | The AppConfig application ID or name. |
| `<environment>` | yes | The AppConfig environment ID or name. |
| `<profile>` | yes | The configuration profile ID or name. |
| `#json-key` | no | Select one field from a JSON configuration payload (via `mamori.SelectKey`). |
| `?minPoll=<seconds>` | no | Sets `RequiredMinimumPollIntervalInSeconds` on the session. It only matters on the (not-yet-implemented) `Watch` path; `Resolve` accepts and ignores it. |

Each of the three path segments may be either the resource's AWS-assigned ID or its name; the provider passes them through verbatim and lets AppConfig Data resolve them.

**Examples**

- `aws-appconfig://myapp/prod/flags` returns the whole configuration payload for the `flags` profile.
- `aws-appconfig://myapp/prod/flags#/db/port` selects the `port` field nested under `db` in a JSON configuration.

```go
type Config struct {
	Flags string `source:"aws-appconfig://myapp/prod/flags"`         // whole configuration payload
	Port  int    `source:"aws-appconfig://myapp/prod/flags#/db/port"` // one field of a JSON configuration
}
```

AppConfig values are never marked `Sensitive`: AppConfig is a configuration service, not a secret store, so nothing about its payloads warrants secret-hygiene treatment by default. Store secrets in Secrets Manager or Parameter Store `SecureString` and reference them from your AppConfig-managed configuration instead. `Value.Version` is the configuration profile's `VersionLabel` when the source is an AppConfig-hosted configuration version, falling back to `mamori.VersionHash` for every other configuration source (Parameter Store, SSM documents, Secrets Manager, S3, or Feature Flags), which have no such label.

### Why `Resolve` costs two API calls

AppConfig Data is a session protocol, not a plain request/response API: a caller first opens a configuration session with `StartConfigurationSession`, then polls it with `GetLatestConfiguration`. A session that already holds the current version receives an *empty* payload from `GetLatestConfiguration` - that's how the protocol tells a long-lived poller "nothing changed."

A provider that opened one session and reused it across `Resolve` calls would therefore return the configuration on the first call and empty bytes on every call after, and mamori would apply those empty bytes over a live configuration field - a silent, hard-to-notice config wipe. `Resolve` avoids this entirely by starting a fresh session and discarding it on every call: a session created moments ago holds no version at all, so the empty-payload case can never legitimately occur on this path. The cost is one extra API call per `Resolve`, which is the price of a stateless, always-correct `Resolve`.

## Explicit configuration

```go
import awsprov "github.com/xavidop/mamori/providers/aws"

mamori.WithProvider(awsprov.NewAppConfig(awsprov.WithRegion("eu-west-1")))
```

## Watch

AppConfig has no native change notification for this provider (Task 1 of its rollout implements only `Resolve`), so mamori polls (`WithPollInterval` + jitter, `Value.Version` comparison).

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

`InternalServerException` and `BadRequestException` are AppConfig Data's own error codes: AppConfig spells its server error differently from the `InternalServerError` the other two schemes in this module use, and `BadRequestException` is what a reused or expired configuration session token comes back as. A missing application, environment, or profile is reported as `ResourceNotFoundException` at `StartConfigurationSession` time, since AppConfig Data resolves identifiers at session start rather than at fetch time.

Codes not listed above report `unknown` rather than being guessed at. The original SDK error stays reachable with `errors.As`.

Verified by unit tests against an in-memory fake that models the AppConfig Data session protocol (single-use tokens, rejection of reused tokens, empty payload on an unchanged version), and the `providertest` conformance kit against the same fake.
