---
layout: ../../../layouts/DocsLayout.astro
title: PostgreSQL provider
---

# PostgreSQL

Read a config or secret value from a Postgres table, with **native watch** via `LISTEN`/`NOTIFY`. Built on `pgx/v5`.

| | |
| --- | --- |
| Scheme | `postgres://` |
| Module | `github.com/xavidop/mamori/providers/postgres` |
| Sensitive | no (opt-in with `WithSensitive`) |
| Watch | native (LISTEN/NOTIFY) |
| Auth | `DATABASE_URL` (or `WithDSN`) |

## Install

```bash
go get github.com/xavidop/mamori/providers/postgres
```

```go
import _ "github.com/xavidop/mamori/providers/postgres"
```

## Using the ref

A `postgres://` ref points at one row of a table (a key/value lookup), optionally selecting a field from a JSON value.

```text
postgres://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<table>` | yes | The table to read. May be schema-qualified with a single dot (`public.settings`). Validated against a strict identifier allowlist. |
| `<key>` | yes | The row key, bound as the `$1` query parameter (never interpolated) and matched with `WHERE <key_col> = $1`. |
| `#json-field` | no | Parse the value column as a JSON object and return one field (via `mamori.SelectKey`). |
| `?key_col=<c>` | no | Override the key column name (default `key`). |
| `?val_col=<c>` | no | Override the value column name (default `value`). |

**Examples**

- `postgres://settings/greeting` - runs `SELECT value FROM settings WHERE key = $1` with `$1 = 'greeting'`.
- `postgres://settings/http?val_col=int_value` - reads the same row but from the `int_value` column instead of `value`.
- `postgres://settings/db#host` - reads the JSON object at key `db` and returns its `host` field.
- `postgres://public.settings/feature_x?key_col=name&val_col=data` - runs `SELECT data FROM public.settings WHERE name = $1`.

```go
type Config struct {
	Greeting string `source:"postgres://settings/greeting"`
	Timeout  int    `source:"postgres://settings/http?val_col=int_value"`
	DBPass   secret.String `source:"postgres://secrets/db_password"`
}
```

The row key is always bound as the `$1` parameter, while the table and column names are validated against a strict identifier allowlist (`^[A-Za-z_][A-Za-z0-9_]*$`, with one optional schema dot) before any query is built, so a ref can never be used for SQL injection. `Value.Version` is a content hash of the value by default, or the column named by `WithVersionColumn` (cast to text) for exact native change detection. Values are non-sensitive unless you set `WithSensitive(true)` or wrap the field in `secret.String`.

## Watch

`Watch` issues `LISTEN <channel>` (default `mamori_config`) and re-queries on each `NOTIFY`. Have your writes signal the channel, for example with a trigger:

```sql
CREATE OR REPLACE FUNCTION mamori_notify() RETURNS trigger AS $$
BEGIN PERFORM pg_notify('mamori_config', NEW.key); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER settings_notify AFTER INSERT OR UPDATE ON settings
FOR EACH ROW EXECUTE FUNCTION mamori_notify();
```

## Configuration

```go
import pgprov "github.com/xavidop/mamori/providers/postgres"

mamori.WithProvider(pgprov.New(pgprov.WithDSN(os.Getenv("DATABASE_URL"))))
```

`Close()` is idempotent and terminal: after it returns, every `Resolve`, and any `Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting the database. It closes the `pgxpool.Pool` this provider opened lazily. A pool injected with `WithPool` belongs to the caller and is left open; `New` followed by `Close` with no prior `Resolve` never dials, so there is nothing to close.

A `Watch` that was **already running** when `Close` was called is a different case and is **not** covered by that guarantee. `Watch` captures its backend once, before its loop starts, and never passes the closed gate again, so `Close` neither ends it nor makes it report `ErrUnavailable`. With a self-opened pool it degrades to an error stream, but the error is the pool's own unclassified "closed pool" failure rather than `mamori.ErrUnavailable`, so `errors.Is(err, mamori.ErrUnavailable)` is **false** for it. With a pool injected through `WithPool`, `Close` never touches the pool at all and the watch keeps serving live values. Cancelling that watch's own context is the only reliable way to shut it down. See [Close does not stop a Watch](/docs/writing-a-provider/#close-does-not-stop-a-watch) for what every other provider does here.

## Error classification

Beyond the not-found case, query failures are classified by SQLSTATE so `mamori.ErrorKind` can distinguish them:

| SQLSTATE | mamori kind |
|---|---|
| `42501` (insufficient_privilege) | `permission_denied` |
| `28P01` (invalid_password), `28000` (invalid_authorization_specification) | `unauthenticated` |
| `53300` (too_many_connections), `57P03` (cannot_connect_now), `08006`, `08001`, `08004` (connection failures) | `unavailable` |
| anything else | `unknown` |

PostgreSQL has no rate-limit SQLSTATE class, so nothing maps to `rate_limited`. Codes not listed above report `unknown` rather than being guessed at, and the original `*pgconn.PgError` stays reachable with `errors.As`. Because the pool is lazy, this is what turns a bad password or a refused connection, which only surface at query time, into a diagnosable kind instead of a bare `unknown`.

Verified with an in-memory fake (including a test that the identifier allowlist rejects malicious names); live behavior is covered by `//go:build integration` tests.
