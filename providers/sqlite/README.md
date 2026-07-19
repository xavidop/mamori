# mamori - SQLite provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from a **SQLite** database table, using the pure-Go
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) driver - **no cgo**,
so it cross-compiles and runs anywhere Go does. It supports **native hot-reload**
by watching the database file with `fsnotify`.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/sqlite"
```

Importing the package registers the `sqlite` scheme with mamori. The database is
opened lazily on first use (path from `SQLITE_PATH`), so importing the package
never touches the filesystem.

## Scheme

```
sqlite://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
```

- `<table>` - the table to read from.
- `<key>` - the key to look up. It is bound as a **query parameter**, never
  interpolated, so it is injection-safe and may contain any characters
  (including slashes after the first).
- `#json-field` - optional. When present the stored value is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (identical
  behavior to every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.
- `?key_col=<c>` / `?val_col=<c>` - optional column-name overrides. Defaults are
  `key_col=key` and `val_col=value`.

The query executed is:

```sql
SELECT <val_col> FROM <table> WHERE <key_col> = ?   -- key bound as parameter
```

> **Injection safety.** SQL cannot parameterize identifiers, so `<table>`,
> `<val_col>`, `<key_col>` (and any `WithVersionColumn`) are validated against a
> strict allowlist `^[A-Za-z_][A-Za-z0-9_]*$` before being interpolated. Anything
> else (spaces, quotes, semicolons, comment markers, ...) is **rejected** and the
> query never runs. Only the *key value* is dynamic, and it is always bound as a
> `?` parameter.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `sqlite://config/log_level` | `value` where `key = 'log_level'` in table `config` |
| `sqlite://config/db#host` | Field `host` of the JSON object stored at key `db` |
| `sqlite://config/db#password` | Field `password` of that JSON object |
| `sqlite://settings/region?key_col=name&val_col=val` | `val` where `name = 'region'` in table `settings` |

```go
type Config struct {
    LogLevel   string `source:"sqlite://config/log_level"`
    DBHost     string `source:"sqlite://config/db#host"`
    DBPassword string `source:"sqlite://config/db#password"`
    Region     string `source:"sqlite://settings/region?key_col=name&val_col=val"`
}
```

### Value semantics

- `Value.Bytes` - the raw column value (after optional `#json-field` selection).
  TEXT/BLOB pass through; INTEGER/REAL/BOOL/`time` values are rendered to their
  canonical text; `NULL` becomes empty bytes.
- `Value.Version` - `mamori.VersionHash` of the raw column value, giving cheap
  change detection without a native revision. Point the provider at a real
  revision column with `WithVersionColumn` to use that column's value verbatim
  instead.
- `Value.Sensitive` - `false` by default (SQLite typically holds configuration,
  not managed secrets). Set `WithSensitive(true)` to mark every value sensitive,
  or wrap individual fields in `secret.String`.
- A missing row (or a missing `#json-field`) returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`.

## Configuration

The **database file path** is provider configuration, not part of the ref, so
refs stay portable across environments.

| Source | How |
| --- | --- |
| `SQLITE_PATH` env var | Default when no option is given |
| `sqlite.WithPath("/var/lib/app.db")` | Explicit file path |
| `sqlite.WithDSN("file:/var/lib/app.db?_pragma=busy_timeout(5000)&mode=ro")` | Full driver DSN (custom pragmas, read-only, shared cache) |

```go
p := sqlite.New(
    sqlite.WithPath("/var/lib/app/config.db"),
    sqlite.WithSensitive(false),
    sqlite.WithVersionColumn("updated_at"), // optional native revision column
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

### Options

| Option | Effect |
| --- | --- |
| `WithPath(path)` | SQLite database file path (default `SQLITE_PATH`) |
| `WithDSN(dsn)` | Full `modernc.org/sqlite` DSN, bypassing path construction |
| `WithVersionColumn(col)` | Use `col`'s value as `Value.Version` instead of hashing the bytes |
| `WithSensitive(true)` | Mark every resolved value `Sensitive` (redacted downstream) |
| `WithDebounce(d)` | Coalesce filesystem events before re-querying on watch (default 150ms) |

The default DSN opens the file with `PRAGMA busy_timeout(5000)` so a read that
races an external writer waits briefly rather than failing immediately.

## Native watch (fsnotify)

The provider implements `mamori.WatchableProvider` by watching the database file
on disk with [`fsnotify`](https://github.com/fsnotify/fsnotify) - the same
mechanism as mamori's built-in `file://` provider:

1. On `Watch`, the current value is emitted immediately as a baseline.
2. The provider watches the database file (and its parent directory, to catch
   atomic replace via rename). On a write it **re-queries** the ref and emits an
   `Update`.
3. SQLite touches the main database file plus its rollback journal on each
   commit, producing a burst of events; these are coalesced with a short
   **debounce** (`WithDebounce`, default 150ms) so a commit yields a single
   re-query.
4. When the watch context is cancelled the watcher is closed, the goroutine
   exits, and the channel is closed - no goroutine leaks (verified with
   `goleak`).

> **Use the default journal mode for watching.** The provider opens the database
> in the default rollback-journal mode so committed writes modify the *main*
> database file in place, which is what `fsnotify` observes. **WAL mode** routes
> writes to a side `-wal` file and only updates the main file on checkpoint,
> which would defer or hide change notifications - avoid WAL (do not pass
> `journal_mode=WAL` via `WithDSN`) if you rely on the native watch.
>
> A watch needs a real file path: it works with `WithPath` / `SQLITE_PATH`, but
> not with a `WithDSN`-only configuration (e.g. `:memory:` or a bare DSN), since
> there is no file to watch.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against a real temporary SQLite file (`go test ./...`, no cgo, no external service) |
| Resolve, custom columns, JSON `#field`, not-found, version monotonicity, context cancellation | **Verified** (unit tests) |
| `WithVersionColumn`, `WithSensitive`, `SQLITE_PATH` fallback | **Verified** (unit tests) |
| Identifier allowlist rejects malicious table / column / version-column names | **Verified** (unit test, with a surviving-canary-row assertion) |
| Native fsnotify watch (baseline + change + cancel/close, no goroutine leak) | **Verified** against a real file |
| End-to-end against a large / production database | **Needs a live database** - the unit tests already use a real embedded SQLite file, so behavior is exercised end-to-end locally |

Because `modernc.org/sqlite` is a pure-Go embedded database, the entire test
suite - including the conformance kit and the native watch - runs against a real
database file with **no external service and no cgo**.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/sqlite
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
