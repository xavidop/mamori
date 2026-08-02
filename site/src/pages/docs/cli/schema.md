---
layout: ../../../layouts/DocsLayout.astro
title: mamori schema
---

# mamori schema

Emits a JSON Schema (draft 2020-12) derived from each qualifying struct's field types and `validate:` tags, ready to feed straight into a JSON Schema validator.

```bash
mamori schema [patterns...] [--type=Name]
```

- `--type=Name` narrows to one struct by name.
- If exactly one struct qualifies, the output is a single schema document. If more than one qualifies, it is a JSON array of documents, each carrying a `title` of `package.TypeName`.

## Schema output

```bash
$ mamori schema ./... --type=Config
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "main.Config",
  "type": "object",
  "properties": {
    "Redis": {
      "type": "object",
      "properties": {
        "Addr": { "type": "string" },
        "Password": { "type": "string" }
      },
      "required": ["Addr", "Password"]
    },
    "Timeout": { "type": "integer", "minimum": 1, "default": 30 }
  }
}
```

## How validate tags map

| `validate:` rule | JSON Schema |
| --- | --- |
| `required` | field listed in the object's `required` array |
| `oneof=a b c` | `enum` |
| `gte` / `lte` (numeric only) | `minimum` / `maximum` |
| `min` / `max` on a number field | `minimum` / `maximum` |
| `min` / `max` on a string field | `minLength` / `maxLength` |

A field is also `required` when it has no `default:` and is not `optional:"true"`. A `default:` tag becomes the schema's `default`, typed as a JSON number where the field is numeric.

## What this describes

`schema` describes the **validated config struct**, after every field is resolved and every `WithDerive` hook has run, not a document you would ever hand mamori. That is why a field with no `source:` tag can appear at all, and even end up `required`: mamori's validator runs against the whole struct, so a field carrying `validate:` rules is enforced whether or not it has a source, and `schema` emits it accordingly. That is also why this output can list a field [`mamori explain`](/docs/cli/explain/) does not: explain lists what mamori reads from a backend, schema lists everything mamori enforces. A `WithDerive`-declared field lands in `required` the same way, and that is correct, not a bug: mamori genuinely rejects a config whose derived field comes out empty after the hook runs.

## Custom provider schemes

Sensitivity is computed from the same [built-in scheme set `mamori vet` uses](/docs/cli/vet/#what-it-flags), which only knows the providers mamori ships. If you [wrote your own provider](/docs/writing-a-provider/), name its scheme so its fields are treated as secrets here too:

```bash
mamori schema --secret-schemes=mysecrets ./...
```

The flag adds to the built-in set rather than replacing it, takes a bare scheme token (not a full ref), and is accepted by `explain`, `schema`, `policy`, `vet`, and `doctor --compare`, so every command agrees on what counts as a secret.

## See also

[`mamori explain`](/docs/cli/explain/) lists the same structs as a table. [CLI overview](/docs/cli/).
