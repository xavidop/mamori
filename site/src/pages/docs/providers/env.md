---
layout: ../../../layouts/DocsLayout.astro
title: env provider
---

# env

Reads process environment variables. Built into the core module and **registered automatically** - no import needed.

| | |
| --- | --- |
| Scheme | `env:` |
| Module | core (built-in) |
| Sensitive | no |
| Watch | poll |
| Auth | none |

## Using the ref

An `env:` ref identifies one process environment variable by name.

```text
env:NAME
```

| Part | Required | What it means |
| --- | --- | --- |
| `env:` | yes | Opaque scheme - everything after the colon is taken literally, with no `//` authority. |
| `NAME` | yes | The environment variable to read, e.g. `LOG_LEVEL`. |

**Examples**

- `env:LOG_LEVEL` reads `$LOG_LEVEL`; pair it with `default:` and a `validate:"oneof=..."` rule so a missing or mistyped value is caught early.
- `env:WORKERS` reads `$WORKERS` into an `int` - add `validate:"gte=1,lte=256"` to bound it.
- `env:AWS_REGION` with `optional:"true"` leaves the field at its zero value when the variable is unset.

```go
type Config struct {
	LogLevel string `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers  int    `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	Region   string `source:"env:AWS_REGION" optional:"true"`
}

cfg, err := mamori.Load[Config](ctx) // env is already registered
```

## Notes

- An unset variable resolves to not-found, so `default:` or `optional:"true"` applies.
- `Value.Version` is a hash of the value, so a watcher notices changes across a re-exec or an explicit `os.Setenv` in tests.
- Environment values are treated as non-sensitive. If a variable holds a secret, use `secret.String` for the field anyway so it stays out of logs.

Environment variables rarely change in a running process, so mamori polls on `WithPollInterval`.
