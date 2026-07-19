---
layout: ../../../layouts/DocsLayout.astro
title: DynamoDB provider
---

# DynamoDB

Read an item attribute from an Amazon DynamoDB table, built on `aws-sdk-go-v2`.

| | |
| --- | --- |
| Scheme | `dynamodb://` |
| Module | `github.com/xavidop/mamori/providers/dynamodb` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | poll |
| Auth | default AWS credential chain (`WithRegion`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/dynamodb
```

```go
import _ "github.com/xavidop/mamori/providers/dynamodb"
```

## Using the ref

A `dynamodb://` ref points at one item in a table, fetched by its primary key with a single `GetItem`.

```text
dynamodb://<table>/<pk>[#attr][?pk_name=<name>&sk=<value>&sk_name=<name>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<table>` | yes | The DynamoDB table name. |
| `<pk>` | yes | The partition-key value (a string). |
| `#attr` | no | Return one top-level attribute instead of the whole item. Without it, the whole item comes back as JSON. |
| `?pk_name=` | no | The partition-key attribute name. Defaults to `pk`. |
| `?sk=` | no | The sort-key value, for a table with a composite primary key. |
| `?sk_name=` | no | The sort-key attribute name. Defaults to `sk`. |

**Examples**

- `dynamodb://app-config/service` reads the item whose `pk` is `service` and returns the whole item as JSON.
- `dynamodb://app-config/service#endpoint` returns just the `endpoint` attribute of that item.
- `dynamodb://services/payments#url?pk_name=service_id` looks the item up by a `service_id` partition key instead of the default `pk`.
- `dynamodb://events/e-1#payload?sk=2024&sk_name=year` reads an item with a composite key (partition `pk` = `e-1`, sort `year` = `2024`).

```go
type Config struct {
	Endpoint string `source:"dynamodb://app-config/service#endpoint"`
	Whole    string `source:"dynamodb://app-config/service"` // whole item as JSON
}
```

`#attr` selects a top-level DynamoDB attribute, not a nested JSON key: a scalar comes back stringified (`S`/`N` verbatim, `BOOL` as `true`/`false`, `NULL` as `null`), while maps, lists, and sets are JSON. `Value.Version` is the item's `version` attribute when present (bump it to force a refresh), otherwise a content hash. Items are not marked sensitive by default; construct the provider with `WithSensitive` when a table holds secret material.

## Watch

mamori polls (`WithPollInterval` + jitter). For push, DynamoDB Streams can drive an on-demand reload in your app.

## Configuration

```go
import ddbprov "github.com/xavidop/mamori/providers/dynamodb"

mamori.WithProvider(ddbprov.New(ddbprov.WithRegion("eu-west-1")))
```

Verified with an in-memory fake; live behavior is covered by `//go:build integration` tests.
