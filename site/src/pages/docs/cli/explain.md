---
layout: ../../../layouts/DocsLayout.astro
title: mamori explain
---

# mamori explain

Prints every struct type with at least one `source:`-tagged field: its field paths, Go types, source chains, defaults, and which fields are sensitive. It reads Go source only and resolves nothing.

```bash
mamori explain [patterns...] [--type=Name] [--json]
```

- `patterns` are Go package patterns (default: the current directory, e.g. `./...`).
- `--type=Name` narrows to one struct by name.
- `--json` emits the same data as JSON instead of a table.

## Table output

One row per field, under a `package.TypeName` banner:

```bash
$ mamori explain ./...
main.Config
FIELD           TYPE           CHAIN                   DEFAULT  OPTIONAL  SENSITIVE
Redis.Addr      string         env://REDIS_ADDR        -        false     false
Redis.Password  secret.String  aws-sm://prod/redis-pw  -        false     true
Timeout         int            env://TIMEOUT           30       false     false
```

## Columns

| Column | Meaning |
| --- | --- |
| `FIELD` | Dotted field path, e.g. `Redis.Password` |
| `TYPE` | The field's Go type, e.g. `secret.String` |
| `CHAIN` | The `source:` tag's comma-separated ref chain, in precedence order (see [Source chains](/docs/concepts/source-chains/)) |
| `DEFAULT` | The `default:` tag's value, or `-` if none |
| `OPTIONAL` | Whether the field carries `optional:"true"` |
| `SENSITIVE` | Whether the field is `secret.String`/`secret.Bytes`, or any ref in its chain uses a secret-bearing scheme |

## See also

[`mamori schema`](/docs/cli/schema/) turns the same structs into a JSON Schema; [`mamori policy`](/docs/cli/policy/) turns their refs into an access artifact. [CLI overview](/docs/cli/).
