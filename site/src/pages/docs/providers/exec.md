---
layout: ../../../layouts/DocsLayout.astro
title: exec provider
---

# exec

Runs a command and uses its standard output as the value. Built into the core module but **disabled by default** - executing commands from configuration is a meaningful attack surface, so you must opt in.

| | |
| --- | --- |
| Scheme | `exec:` |
| Module | core (opt-in) |
| Sensitive | yes |
| Watch | poll |
| Auth | none |

## Enabling it

`exec:` is not auto-registered. Enable it for a single `Load` / `Watch` call with `WithExecProvider`:

```go
type Config struct {
	Token secret.String `source:"exec:vault-agent token"`
}

cfg, err := mamori.Load[Config](ctx, mamori.WithExecProvider())
```

## Using the ref

An `exec:` ref names a command line; mamori runs it and uses its standard output as the value.

```text
exec:command arg1 arg2 ...
```

| Part | Required | What it means |
| --- | --- | --- |
| `exec:` | yes | Opaque scheme - the entire remainder is the command line, with no `//` authority. |
| `command` | yes | The executable to run (resolved on `PATH`). |
| `arg1 arg2 ...` | no | Arguments, split on spaces. Taken verbatim from the ref, never interpolated from other resolved values. |

**Examples**

- `exec:vault-agent token` runs `vault-agent token` and captures its stdout as the value - pair it with a `secret.String` field.
- `exec:aws ecr get-login-password` shells out to the AWS CLI to mint a short-lived registry password.

The `exec:` scheme must be enabled per call with `WithExecProvider()` (see above), and its output is always marked `Sensitive`. See Security below.

## Security

- Disabled unless you call `WithExecProvider()`.
- The command is taken verbatim from the ref and is **never interpolated from other resolved values**, so one secret cannot be used to build another's command (no injection chains).
- Output is marked `Sensitive`. A non-zero exit status becomes an error (last-good value is retained under `Watch`).

Because there is no native change signal, mamori re-runs the command on `WithPollInterval`.
