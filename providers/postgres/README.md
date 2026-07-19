# mamori - PostgreSQL provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from a **PostgreSQL table**, with **native hot-reload** driven by
PostgreSQL `LISTEN`/`NOTIFY`.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/postgres"
```

Importing the package registers the `postgres` scheme with mamori. The connection
pool is built lazily on first use from `DATABASE_URL`, so importing the package
never contacts the database.

## Scheme

```
postgres://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

- `<table>` - the table to read from. May be schema-qualified with a single dot
  (`public.app_config`). Validated against a strict identifier allowlist (see
  [Identifier safety](#identifier-safety)).
- `<key>` - the row key. It is bound as the `$1` **query parameter**, never
  interpolated into SQL, so it may contain any characters (including `/`).
- `#json-field` - optional. When present, the value column is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (identical to
  every other mamori provider). String fields are returned unquoted; objects,
  arrays, numbers, and booleans are returned as their JSON encoding.
- `?key_col=` / `?val_col=` - optional per-ref overrides of the key and value
  column names. Defaults: `key_col=key`, `val_col=value`.

Each resolution runs a single parameterized query:

```sql
SELECT <val_col> FROM <table> WHERE <key_col> = $1   -- $1 = <key>
```

### Ref examples

| Ref | Query |
| --- | --- |
| `postgres://app_config/log_level` | `SELECT value FROM app_config WHERE key = $1` (`$1='log_level'`) |
| `postgres://app_config/db#host` | as above, then JSON field `host` of the value |
| `postgres://public.settings/feature_x` | `SELECT value FROM public.settings WHERE key = $1` |
| `postgres://settings/feature_x?key_col=name&val_col=data` | `SELECT data FROM settings WHERE name = $1` |

```go
type Config struct {
    LogLevel   string `source:"postgres://app_config/log_level"`
    DBHost     string `source:"postgres://app_config/db#host"`
    DBPassword string `source:"postgres://app_config/db#password"`
    FeatureX   string `source:"postgres://settings/feature_x?key_col=name&val_col=data"`
}
```

### Value semantics

- `Value.Bytes` - the raw value-column bytes. `text`, `jsonb`, and `bytea`
  columns all scan cleanly into bytes.
- `Value.Version` - by default a hash of the value bytes (`mamori.VersionHash`),
  giving cheap change detection with no extra column. Set
  [`WithVersionColumn`](#options) to use a revision counter or `updated_at`
  timestamp instead (the column is cast to `text`, so any column type works).
- `Value.Sensitive` - `false` by default (a config table is not a managed secret
  store). Construct with `WithSensitive(true)` to mark every value secret, or
  wrap individual fields in `secret.String`.
- A missing row returns an error satisfying `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

By default the provider reads the connection string from the `DATABASE_URL`
environment variable (standard libpq / pgx URL or keyword form):

```
DATABASE_URL=postgres://user:pass@host:5432/dbname?sslmode=require
```

For explicit configuration, construct the provider yourself and register it:

```go
p := postgres.New(
    postgres.WithDSN(os.Getenv("MY_PG_URL")),
    postgres.WithVersionColumn("updated_at"),
    postgres.WithChannel("mamori_config"),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom pool (custom TLS, pool sizing, tracing):

```go
pool, _ := pgxpool.New(ctx, dsn)
p := postgres.New(postgres.WithPool(pool))
```

### Options

| Option | Effect |
| --- | --- |
| `WithDSN(dsn)` | Connection string (overrides `DATABASE_URL`) |
| `WithPool(*pgxpool.Pool)` | Inject a pre-configured pool |
| `WithKeyColumn(col)` | Default key column name (default `key`); `?key_col=` overrides per-ref |
| `WithValueColumn(col)` | Default value column name (default `value`); `?val_col=` overrides per-ref |
| `WithVersionColumn(col)` | Use this column (cast to `text`) as `Value.Version` instead of a value hash |
| `WithChannel(name)` | `LISTEN`/`NOTIFY` channel for `Watch` (default `mamori_config`) |
| `WithSensitive(true)` | Mark every resolved value `Sensitive` (drives redaction) |

## Native watch (LISTEN/NOTIFY)

The provider implements `mamori.WatchableProvider` using PostgreSQL
`LISTEN`/`NOTIFY` - the idiomatic push mechanism, not a polling ticker:

1. On `Watch`, the current value is emitted immediately as a baseline.
2. The provider acquires a dedicated pooled connection and issues
   `LISTEN <channel>` (default channel `mamori_config`).
3. It then blocks on `WaitForNotification`. When your database issues a
   `NOTIFY <channel>` (typically from a trigger), the provider re-queries the ref
   and emits a fresh `Update`.
4. Transient errors are delivered as `Update{Err: ...}` and the loop retries
   after a short backoff.
5. When the watch context is cancelled the listening connection is closed, the
   goroutine exits, and the channel is closed - no goroutine leaks (verified with
   `goleak`).

> **The database must `NOTIFY` on change.** PostgreSQL does not push row changes
> on its own; you install a trigger that fires `pg_notify` whenever the config
> table is written. The `Watch` payload is ignored - any `NOTIFY` on the channel
> triggers a re-query of the watched ref.

### Sample trigger

```sql
-- config table: keyed rows the provider reads.
CREATE TABLE app_config (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Fire a NOTIFY on the "mamori_config" channel for every insert/update.
CREATE OR REPLACE FUNCTION app_config_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('mamori_config', NEW.key);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER app_config_notify_trg
    AFTER INSERT OR UPDATE ON app_config
    FOR EACH ROW EXECUTE FUNCTION app_config_notify();
```

The channel name (`mamori_config` above) must match `WithChannel` (or its
default). If you pair the trigger with an `updated_at` column, point the provider
at it with `WithVersionColumn("updated_at")` for exact native change detection.

## Identifier safety

The row key is always bound as the `$1` **query parameter** and is never part of
the SQL text, so it cannot inject SQL. Table and column **names**, however,
cannot be parameterized by SQL. The provider therefore validates every
identifier (the table, the key column, the value column, the optional version
column, and the `LISTEN` channel) against a strict allowlist **before building
any query**:

- a single identifier must match `^[A-Za-z_][A-Za-z0-9_]*$`;
- a table may be schema-qualified with **exactly one** dot (`schema.table`).

Anything else - whitespace, quotes, semicolons, parentheses, comment markers, a
second dot - is rejected with an error (distinct from `ErrNotFound`, so a
rejected injection attempt is never mistaken for a missing key) and no query
runs. This is the SQL-injection boundary for identifiers.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake backend (`go test ./...`) |
| Resolve, JSON `#key`, custom `key_col`/`val_col`, schema-qualified table, not-found, context cancellation | **Verified** (unit tests) |
| Version hash vs. `WithVersionColumn` | **Verified** (unit tests) |
| Identifier allowlist rejects malicious table/column/channel names | **Verified** (unit tests) |
| Native `LISTEN`/`NOTIFY` watch (baseline + NOTIFY-driven change + cancel/close, no goroutine leak) | **Verified** against the fake |
| End-to-end against a real PostgreSQL server (real `LISTEN`/`NOTIFY` + trigger) | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces the
row-lookup and NOTIFY-wakes-a-waiter semantics, so `go test ./...` requires
**no** running PostgreSQL.

### Live integration test

An integration test exercises a real PostgreSQL server (creating a table with the
NOTIFY trigger above). It is guarded by a build tag and skips unless
`DATABASE_URL` is set:

```sh
docker run --rm -e POSTGRES_PASSWORD=pass -p 5432:5432 postgres:16
export DATABASE_URL='postgres://postgres:pass@127.0.0.1:5432/postgres?sslmode=disable'
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/postgres
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
