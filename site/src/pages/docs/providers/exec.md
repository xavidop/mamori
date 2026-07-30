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
| `arg1 arg2 ...` | no | Arguments, split on whitespace. Quote an argument to keep spaces in it. Taken verbatim from the ref, never interpolated from other resolved values. |

**Examples**

- `exec:vault-agent token` runs `vault-agent token` and captures its stdout as the value - pair it with a `secret.String` field.
- `exec:aws ecr get-login-password` shells out to the AWS CLI to mint a short-lived registry password.
- `exec:mytool --msg "hello world"` passes one argument containing a space.

The `exec:` scheme must be enabled per call with `WithExecProvider()` (see above), and its output is always marked `Sensitive`. See Security below.

### Quoting

Arguments split on whitespace, and single or double quotes keep an argument together. Single quotes are literal; inside double quotes a backslash escapes the next character. An unterminated quote is an error rather than a guess, since closing it silently would run a command you did not write.

### There is no shell

mamori runs the binary directly. There is no globbing, no pipes, no command substitution, and **no variable expansion**:

```go
// Does NOT work: no shell, so $HOME is passed as the literal text "$HOME".
Home string `source:"exec:echo $HOME"`
```

That one produces the string `$HOME` and no error, which is the failure most worth knowing about on this page.

### Using an environment variable

Three ways, depending on what you actually need.

**The value itself.** Do not use `exec:` at all:

```go
Home string `source:"env:HOME"`
```

**The command should see your environment.** It already does. mamori does not clear the environment, so the command inherits it and a script reading `$HOME` internally works:

```go
Token secret.String `source:"exec:vault-agent token"`
```

**The variable must appear in the command line.** Two routes, and they differ in who does the substituting.

Let mamori substitute it, with [`${VAR}` interpolation](/docs/concepts/ref-interpolation/). You name the variables explicitly, so nothing ambient can reach the command:

```go
Home string `source:"exec:printf %s ${HOME}"`

cfg, err := mamori.Load[Config](ctx,
	mamori.WithExecProvider(),
	mamori.WithRefVars(mamori.EnvVars("HOME")),
)
```

Or ask for a shell yourself, quoting the script so it arrives as one argument:

```go
Home string `source:"exec:sh -c 'echo $HOME'"`
```

Prefer the first. `${HOME}` fails loudly at `Load` if you forget to pass the variable, whereas a shell silently expands an unset variable to nothing. Invoking a shell is also your decision to make, not something mamori does to every `exec:` ref, and anything that shell can expand, it will.

### Trailing newlines

Whatever the command prints becomes the value, newline included: `echo` gives you `"/home/app\n"`, not `"/home/app"`. That trailing byte fails a `validate:"..."` rule or any comparison expecting an exact match.

Trim it with [`?decode=trim`](/docs/concepts/decoding/), or use a command that emits no newline:

```go
Home  string        `source:"exec:sh -c 'echo $HOME'?decode=trim"`
Token secret.String `source:"exec:printf %s hunter2"`  // printf adds none
```

## Security

- Disabled unless you call `WithExecProvider()`.
- The command is taken verbatim from the ref and is **never interpolated from other resolved values**, so one secret cannot be used to build another's command (no injection chains).
- Output is marked `Sensitive`. A non-zero exit status becomes an error (last-good value is retained under `Watch`).

Because there is no native change signal, mamori re-runs the command on `WithPollInterval`.

## Error classification

| Condition | mamori kind |
|---|---|
| Empty command (nothing after `exec:`) | `invalid` |
| Binary not found on `PATH` | `unknown` |
| Permission denied executing the binary | `permission_denied` |
| Command runs and exits non-zero, or any other failure | `unknown` |

A binary missing from `PATH` reports `unknown`, not `not_found`: it means mamori could not even attempt to fetch the value, not that the value itself is absent, so it must never trigger `default:` or `optional` handling.

A non-zero exit also reports `unknown` because mamori cannot tell whether it failed from a missing value, a permission problem, or a bug in the script.
