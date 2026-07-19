---
layout: ../../../layouts/DocsLayout.astro
title: dotenv provider
---

# dotenv

Read a single variable from a `.env` file and hot-reload it with fsnotify, without touching the process environment. Built into the core module and registered automatically.

| | |
| --- | --- |
| Scheme | `dotenv://` |
| Module | core (built-in) |
| Sensitive | no |
| Watch | fsnotify (native) |
| Auth | filesystem permissions |

## Install

Nothing to install - `dotenv://` ships with the core module and is always registered.

```go
import "github.com/xavidop/mamori"
```

## Using the ref

A `dotenv://` ref points at one variable inside a `.env` file on disk.

```text
dotenv://<path>[#KEY]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<path>` | yes | Path to the `.env` file. Relative (`dotenv://.env`) or absolute with a leading slash (`dotenv:///etc/app/.env`). |
| `#KEY` | no | The variable to read. Without it, the whole file's raw bytes are returned (use `flatten:"env"` to decode it into a struct). |

**Examples**

- `dotenv://.env#DB_PASSWORD` reads `DB_PASSWORD` from a `.env` in the working directory.
- `dotenv:///etc/app/.env#API_KEY` reads `API_KEY` from an absolute path.
- `dotenv://config/.env` returns the whole file - pair it with `flatten:"env"` to decode every variable into a nested struct.

```go
type Config struct {
	DBPassword secret.String `source:"dotenv://.env#DB_PASSWORD"`
	APIKey     secret.String `source:"dotenv:///etc/app/.env#API_KEY"`
}
```

The parser understands a leading `export `, single- and double-quoted values (with the usual `\n` / `\t` / `\"` escapes inside double quotes), full-line `#` comments, and a trailing ` #` comment on unquoted values. `Value.Version` is a hash of the file size and modification time, so an unchanged file is not re-read. A missing file, or a missing `#KEY`, resolves to not-found, so `default:` / `optional:"true"` applies.

Values are not marked `Sensitive` by default; because `.env` files often hold secrets, wrap the field in `secret.String` (as above) so it stays redacted in logs.

## How it relates to `env:` and `file://`

- **`env:NAME`** reads a process environment variable (`os.Getenv`).
- **`dotenv://path#KEY`** reads one variable from a specific `.env` file, hot-reloaded, without polluting the process environment.
- **`file:///path/.env` with `flatten:"env"`** decodes a whole `.env` file into a nested struct.

## Watch

`Watch` uses fsnotify on the file (watching the parent directory to catch atomic renames), re-reading and re-emitting when the `.env` file changes.
