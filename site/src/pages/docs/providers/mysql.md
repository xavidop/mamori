---
layout: ../../../layouts/DocsLayout.astro
title: MySQL provider
---

# MySQL

Read a config or secret value from a MySQL / MariaDB table.

| | |
| --- | --- |
| Scheme | `mysql://` |
| Module | `github.com/xavidop/mamori/providers/mysql` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | poll |
| Auth | DSN (`WithDSN` / `MYSQL_DSN`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/mysql
```

```go
import _ "github.com/xavidop/mamori/providers/mysql"
```

## Using the ref

A `mysql://` ref points at one row of a MySQL or MariaDB table (a key/value lookup), optionally selecting a field from a JSON value.

```text
mysql://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<table>` | yes | The table to read. Validated against a strict identifier allowlist, then backtick-quoted. |
| `<key>` | yes | The row key, bound as a `?` placeholder (never interpolated) and matched with `WHERE <key_col> = ?`. |
| `#json-field` | no | Parse the value column as a JSON object and return one field (via `mamori.SelectKey`). |
| `?key_col=<c>` | no | Override the key column name (default `key`). |
| `?val_col=<c>` | no | Override the value column name (default `value`). |

**Examples**

- `mysql://settings/greeting` - runs `SELECT value FROM settings WHERE key = ?` with the key bound to `greeting`.
- `mysql://settings/workers?val_col=int_value` - reads the same row but from the `int_value` column instead of `value`.
- `mysql://config/db#host` - reads the JSON object at key `db` and returns its `host` field.
- `mysql://settings/feature_x?key_col=name&val_col=data` - runs `SELECT data FROM settings WHERE name = ?`.

```go
type Config struct {
	Greeting string `source:"mysql://settings/greeting"`
	Workers  int    `source:"mysql://settings/workers?val_col=int_value"`
}
```

The row key is always a bound `?` placeholder; the table and column names are validated against a strict identifier allowlist (`^[A-Za-z_][A-Za-z0-9_]*$`) and backtick-quoted before any query runs, so a ref can never inject SQL. `Value.Version` is a content hash of the value by default, or a native revision column via `WithVersionColumn`. Values are non-sensitive unless you set `WithSensitive(true)` or wrap the field in `secret.String`.

## Watch

MySQL has no built-in change notification, so mamori polls (`WithPollInterval` + jitter).

## Configuration

```go
import mysqlprov "github.com/xavidop/mamori/providers/mysql"

mamori.WithProvider(mysqlprov.New(mysqlprov.WithDSN("user:pass@tcp(mysql:3306)/appdb")))
```

Verified with an in-memory fake (including an identifier-allowlist rejection test); live behavior is covered by `//go:build integration` tests.
