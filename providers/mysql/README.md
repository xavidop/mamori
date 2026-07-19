# mamori MySQL provider

[MySQL](https://www.mysql.com/) / [MariaDB](https://mariadb.org/) key/value table
provider for [mamori](https://github.com/xavidop/mamori). Pure `database/sql` +
[`go-sql-driver/mysql`](https://github.com/go-sql-driver/mysql) - no ORM.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```bash
go get github.com/xavidop/mamori/providers/mysql
```

```go
import _ "github.com/xavidop/mamori/providers/mysql" // registers mysql://
```

Importing the package registers the `mysql` scheme with mamori. The database pool
is opened lazily on the first resolve, so importing the package never contacts a
database.

## Scheme

```
mysql://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

Each resolve runs a single parameterized query:

```sql
SELECT `<val_col>` FROM `<table>` WHERE `<key_col>` = ?
```

- `<table>` - the table to read from.
- `<key>` - the lookup key, **always bound as a `?` placeholder** (never
  interpolated).
- `#json-field` - optional. When present, the value column is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same
  behavior as every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.
- `key_col` / `val_col` - optional query options overriding the column names.
  Defaults are `key_col=key` and `val_col=value` (the default `key` column is a
  MySQL reserved word and works because identifiers are backtick-quoted).

### Ref examples

| Ref | Meaning |
| --- | --- |
| `mysql://config/log_level` | `SELECT value FROM config WHERE key = 'log_level'` |
| `mysql://config/db#host` | Field `host` of the JSON object stored at key `db` |
| `mysql://settings/feature_x?key_col=name&val_col=data` | `SELECT data FROM settings WHERE name = 'feature_x'` |

```go
type Config struct {
    LogLevel   string `source:"mysql://config/log_level"`
    DBHost     string `source:"mysql://config/db#host"`
    DBPassword string `source:"mysql://config/db#password"`
    FeatureX   bool   `source:"mysql://settings/feature_x?key_col=name&val_col=data"`
}
```

### Value semantics

- `Value.Bytes` - the raw value column for the row.
- `Value.Version` - a content hash of the row's value (`mamori.VersionHash`) by
  default, so change detection is cheap. If the table has a native revision
  column (a version counter or an `updated_at` timestamp), use
  `mysql.WithVersionColumn("updated_at")` and that column becomes `Value.Version`
  instead.
- `Value.Sensitive` - `false` by default (a config table is not a secret
  manager). Set `mysql.WithSensitive(true)` to redact resolved values downstream,
  or wrap the destination field in `secret.String`.
- A key with no matching row returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)` (from `sql.ErrNoRows`).

## Identifier safety (SQL injection)

Bound parameters can carry the **key value** but not table or column **names**,
so those identifiers must be interpolated into the SQL text. To make that safe,
every identifier - the table, `key_col`, `val_col`, and any version column - is
validated against a strict allowlist before a query is built:

```
^[A-Za-z_][A-Za-z0-9_]*$
```

Anything containing whitespace, punctuation, or SQL metacharacters is **rejected**
with a non-`ErrNotFound` error, so a ref like
`mysql://users;DROP TABLE users/k` or `?val_col=value UNION SELECT password`
never reaches the database. Validated identifiers are additionally backtick-quoted.
The lookup key is always passed as a `?` placeholder and is never part of the SQL
text.

## Authentication & configuration

The database is reached through a `go-sql-driver/mysql`
[DSN](https://github.com/go-sql-driver/mysql#dsn-data-source-name):

```
user:password@tcp(host:3306)/dbname?parseTime=false
```

The DSN is taken from, in order:

1. `mysql.WithDSN("...")`
2. the `DATABASE_URL` environment variable
3. the `MYSQL_DSN` environment variable

Both environment variables must be in **go-sql-driver DSN form** (not a
`mysql://` URL).

```go
p := mysql.New(
    mysql.WithDSN("app:secret@tcp(127.0.0.1:3306)/appdb"),
    mysql.WithVersionColumn("updated_at"), // optional native revision column
    mysql.WithSensitive(true),             // optional: redact resolved values
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a pre-configured pool (custom timeouts, TLS, connection limits):

```go
db, _ := sql.Open("mysql", dsn)
db.SetMaxOpenConns(10)
p := mysql.New(mysql.WithDB(db))
```

### Options

| Option | Effect |
| --- | --- |
| `WithDSN(dsn)` | Set the go-sql-driver DSN explicitly |
| `WithDB(*sql.DB)` | Inject a pre-configured pool (bypasses DSN handling) |
| `WithVersionColumn(col)` | Read `Value.Version` from a native revision column |
| `WithSensitive(bool)` | Mark resolved values as secret (redaction) |

`Provider.Close()` releases a pool the provider opened itself; it is a no-op for
a pool injected with `WithDB` (that pool is owned by the caller).

## Watch

MySQL has no native change-notification mechanism, so this provider is **not**
watchable; mamori wraps it in its polling adapter automatically. Configure the
cadence with `mamori.WithPollInterval` (interval + jitter).

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake query surface (`go test ./...`) |
| Resolve, custom columns, JSON `#key` selection, not-found, version monotonicity, context cancellation | **Verified** (unit tests) |
| Parameterized-key binding and identifier allowlist rejecting a malicious table/column | **Verified** (unit tests) |
| End-to-end against a real MySQL/MariaDB server | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake injected through the
provider's internal query interface, so `go test ./...` requires **no** running
database.

### Live integration test

A `//go:build integration` test exercises a real server. It skips unless
`MYSQL_DSN` (or `DATABASE_URL`) is set:

```sh
docker run --rm -d --name mamori-mysql -e MYSQL_ROOT_PASSWORD=secret \
    -e MYSQL_DATABASE=appdb -p 3306:3306 mysql:8
export MYSQL_DSN='root:secret@tcp(127.0.0.1:3306)/appdb'
GOWORK=off go test -tags=integration -run TestLive ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/mysql
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
