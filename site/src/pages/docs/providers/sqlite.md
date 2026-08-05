---
layout: ../../../layouts/DocsLayout.astro
title: SQLite provider
---

# SQLite

Read a config or secret value from a local SQLite database, hot-reloaded with fsnotify. Uses the pure-Go `modernc.org/sqlite` driver (no cgo).

| | |
| --- | --- |
| Scheme | `sqlite://` |
| Module | `github.com/xavidop/mamori/providers/sqlite` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | fsnotify (native) |
| Auth | file permissions (`WithPath` / `SQLITE_PATH`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/sqlite
```

```go
import _ "github.com/xavidop/mamori/providers/sqlite"
```

## Using the ref

A `sqlite://` ref points at one row of a table in the configured SQLite database file (a key/value lookup), optionally selecting a field from a JSON value. The database file path is provider configuration, not part of the ref.

```text
sqlite://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<table>` | yes | The table to read. Validated against a strict identifier allowlist. |
| `<key>` | yes | The row key, bound as a `?` query parameter (never interpolated) and matched with `WHERE <key_col> = ?`. |
| `#json-field` | no | Parse the value column as a JSON object and return one field (via `mamori.SelectKey`). |
| `?key_col=<c>` | no | Override the key column name (default `key`). |
| `?val_col=<c>` | no | Override the value column name (default `value`). |

**Examples**

- `sqlite://settings/greeting` - reads `value` where `key = 'greeting'` in table `settings`.
- `sqlite://config/db#host` - reads the JSON object at key `db` and returns its `host` field.
- `sqlite://settings/region?key_col=name&val_col=val` - reads `val` where `name = 'region'` in table `settings`.

```go
type Config struct {
	Greeting string `source:"sqlite://settings/greeting"`
}

mamori.WithProvider(sqliteprov.New(sqliteprov.WithPath("/var/lib/app/config.db")))
```

The database file path is not part of the ref (set it with `WithPath` or `SQLITE_PATH`), so refs stay portable across environments. The row key is always a bound `?` parameter; the table and column names are validated against a strict identifier allowlist (`^[A-Za-z_][A-Za-z0-9_]*$`) before any query runs, so a ref can never inject SQL. `Value.Version` is a content hash of the value by default, or a native revision column via `WithVersionColumn`. Values are non-sensitive unless you set `WithSensitive(true)` or wrap the field in `secret.String`.

## Error classification

Beyond the not-found case, a query failure that carries a SQLite result code is classified via `modernc.org/sqlite`'s own `*sqlite.Error.Code()`, so `mamori.ErrorKind` can distinguish it:

| SQLite result code | mamori kind |
| --- | --- |
| `SQLITE_PERM` (3), `SQLITE_READONLY` (8) | `permission_denied` |
| `SQLITE_BUSY` (5), `SQLITE_LOCKED` (6), `SQLITE_CANTOPEN` (14) | `unavailable` |
| `SQLITE_AUTH` (23) | `unauthenticated` |
| anything else | `unknown` |

Codes not listed above report `unknown` rather than being guessed at, and the original `*sqlite.Error` stays reachable with `errors.As`.

`Close()` releases nothing: this provider opens a fresh `*sql.DB` on every `Resolve` and closes it again before returning, so there is never a persistent connection for `Close` to hold. Its only job is to make the provider terminal, and it does that: after `Close`, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without opening a connection to the database file. sqlite keeps this terminal-only `Close` mainly because it sits beside `providers/postgres` and `providers/mysql`; a caller sweeping `Close` across the database providers at shutdown would otherwise be surprised to find sqlite alone still serving.

## Watch

`Watch` uses fsnotify on the database file: when the file changes, mamori re-queries and emits an update. Ideal for a config DB written by another process.

Verified against a real temporary SQLite database (no cgo, no external service). An identifier-allowlist rejection test is included, and error classification is verified both by a table test over real driver errors and by a dedicated Resolve-level test proving the wiring is non-vacuous.
