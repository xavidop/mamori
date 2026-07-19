# mamori DynamoDB provider

Amazon DynamoDB provider for [mamori](https://github.com/xavidop/mamori), built on `aws-sdk-go-v2`. Resolve config values straight from a DynamoDB item.

```bash
go get github.com/xavidop/mamori/providers/dynamodb
```

```go
import _ "github.com/xavidop/mamori/providers/dynamodb" // registers dynamodb://
```

## Scheme

```
dynamodb://<table>/<pk>[#attr][?pk_name=<name>&sk=<value>&sk_name=<name>]
```

| Part | Meaning | Default |
|---|---|---|
| `<table>` | DynamoDB table name | - (required) |
| `<pk>` | partition key **value** (string) | - (required) |
| `#attr` | select one top-level attribute; omit for the whole item | whole item |
| `?pk_name` | partition key **attribute** name | `pk` |
| `?sk` | sort key **value** (string); set only for a composite primary key | - |
| `?sk_name` | sort key **attribute** name | `sk` |

Each ref resolves with a single `GetItem`. A missing item (or a `#attr` that the
item lacks) returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`, so
mamori applies defaults / optional handling.

### What you get back

- **No `#attr`** - the whole item as plain JSON, e.g. `{"pk":"u-42","email":"neo@zion","age":30}`.
- **`#attr`** - that attribute only. Scalars are stringified (`S`/`N` verbatim,
  `BOOL` as `true`/`false`, `NULL` as `null`, `B` as raw bytes); maps, lists, and
  sets are JSON-encoded.

### Examples

```go
type Config struct {
    // Whole item (partition key attribute is "pk"):
    Feature   string `source:"dynamodb://features/checkout"`

    // One attribute of an item:
    Email     string `source:"dynamodb://users/u-42#email"`

    // Custom partition key attribute name:
    Endpoint  string `source:"dynamodb://services/payments#url?pk_name=service_id"`

    // Composite primary key (partition "pk" + sort "year"):
    Payload   string `source:"dynamodb://events/e-1#payload?sk=2024&sk_name=year"`

    // Secret material - mark the value Sensitive (see below).
    APIKey    secret.String `source:"dynamodb://secrets/prod#api_key"`
}
```

`#attr` selects a **top-level DynamoDB attribute** (not a nested JSON key). To
reach into a JSON blob stored in a single string attribute, resolve the
attribute and decode it in your own type.

### Version / change detection

`Value.Version` is taken from a top-level `version` attribute when the item has
one (bump it from your application to force a refresh), otherwise it is a content
hash of the returned bytes (`mamori.VersionHash`). Either way, mamori detects
changes cheaply on the next poll.

### Sensitivity

Items are **not** marked `Sensitive` by default. When a table holds secret
material, construct the provider with `WithSensitive()` so resolved values are
redacted downstream:

```go
mamori.WithProvider(dynamodb.New(dynamodb.WithSensitive()))
```

## Authentication

Uses the standard AWS credential chain (env vars, shared config/profile, IAM
role, SSO, ...). The region comes from the ambient configuration (`AWS_REGION`,
`AWS_DEFAULT_REGION`, shared config, or instance metadata) unless pinned:

```go
mamori.WithProvider(dynamodb.New(dynamodb.WithRegion("eu-west-1")))
```

## Watch

DynamoDB has no cheap native change notification, so this provider does **not**
implement `WatchableProvider` - mamori polls it (interval + jitter, `Value.Version`
comparison). Configure with `mamori.WithPollInterval`.

> **Future push mode:** DynamoDB Streams can deliver item-level change events, but
> they require extra infrastructure (a stream on the table plus a shard-iterator
> consumer, or a Lambda/Kinesis pipeline). A future release may add an opt-in
> Streams-backed `Watch`; today the provider stays zero-infrastructure and relies
> on polling.

## What is verified

- ✅ Unit tests against an injected in-memory fake DynamoDB client, plus the
  [`providertest`](../../providertest) conformance kit against that fake
  (resolution, not-found typing, `Version` monotonicity, concurrency, context
  cancellation, goroutine hygiene).
- ⚠️ Live DynamoDB behavior is exercised by `//go:build integration` tests that
  require real credentials and a provisioned table; they are **not** run in CI by
  default. See `integration_test.go` for the environment variables.

Passes the mamori conformance kit. 🛡️
