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

## Derived fields

A [`WithDerive`](/docs/usage/derived-fields/)-declared field is listed too, found by reading the call site itself rather than a `source:` tag. `CHAIN` is blank, `DEFAULT` is always `-`, and `OPTIONAL` is always `false`, since none of that applies to a field mamori computes rather than resolves; `SENSITIVE` still reflects the Go type, true for `secret.String`/`secret.Bytes`. A write path built at runtime instead of written as a literal, a variable or `paths...` from a slice, can't be recovered this way: a struct with one gets a trailing note that its derived fields may be incomplete. [`mamori policy`](/docs/cli/policy/) grants a derived field nothing, since it has no ref.

## Custom provider schemes

`SENSITIVE` is computed from the same [built-in scheme set `mamori vet` uses](/docs/cli/vet/#what-it-flags), which only knows the providers mamori ships. If you [wrote your own provider](/docs/writing-a-provider/), name its scheme so its fields report as sensitive:

```bash
mamori explain --secret-schemes=mysecrets ./...
```

Pass several as a comma-separated list. The flag adds to the built-in set rather than replacing it, and takes a bare scheme token (`mysecrets`), not a full ref.

## JSON stability

The `--json` output is consumed by [`mamori diff`](/docs/cli/diff), which is
typically given a `base.json` stored as a CI artifact and produced weeks
earlier, possibly by an older `mamori` binary. That makes this output a
compatibility surface, and it is treated as one:

**Fields may be added to this JSON. They will not be removed and will not be
retyped.**

The top level is a JSON array of struct records and will stay an array. Consumers
should ignore unknown fields, which is what `encoding/json` does by default, so
a newer binary's output stays readable by an older consumer and the reverse.

## See also

[`mamori schema`](/docs/cli/schema/) turns the same structs into a JSON Schema; [`mamori policy`](/docs/cli/policy/) turns their refs into an access artifact. [CLI overview](/docs/cli/).
