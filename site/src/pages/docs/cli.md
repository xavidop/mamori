---
layout: ../../layouts/DocsLayout.astro
title: CLI
---

# CLI

`mamori` (import path `github.com/xavidop/mamori/cmd/mamori`) is a standalone CLI built around the same source-tag conventions and `Report` shape the library itself uses. It has two halves that never mix, and knowing which half a command belongs to tells you everything about what it can and cannot do:

- **Static commands** - `explain`, `schema`, `policy` - read Go source with `golang.org/x/tools/go/packages` and extract `source:` refs. They make no network calls, contact no secret manager, and **never resolve anything**: they answer "what does this config struct declare," not "what value does it currently hold."
- **Live commands** - `doctor`, `status` - are thin HTTP clients of a *running* process's admin endpoint (`mamori.WithAdminHTTP` / `mamori.Handler`, see [Observability](../observability)). They read no source and hold no opinion of their own about what a config struct should look like: they only render whatever `mamori.Report` the target process's admin endpoint returns right now.

This is decision D1 in the CLI's design, and it is enforced structurally, not just by convention: the static commands' code path never opens a socket, and the live commands' code path never calls `go/packages`, with one narrow exception (`doctor --compare`, below) that is documented as exactly that - an exception, not a blurring of the line.

## Install

```bash
brew install xavidop/tap/mamori
```

```bash
go install github.com/xavidop/mamori/cmd/mamori@latest
```

Both install the same binary. The Homebrew tap tracks tagged releases built by the project's GoReleaser config; `go install` builds from source at whatever ref you name.

## Static commands

All three static commands accept the same package-pattern argument `go build` does (`./...` to walk a whole module, a specific package path, or nothing at all for the current directory), and load only Go source - no build tags gate a struct in or out beyond it having at least one `source:`-tagged field.

### `mamori explain`

```bash
mamori explain ./... [--type=Name] [--json]
```

Prints every struct type with at least one `source:`-tagged field: its field paths, Go types, source chains, defaults, and which fields are sensitive. `--type` narrows to one struct by name; `--json` emits the same data as JSON instead of a table.

Table columns:

| Column | Meaning |
| --- | --- |
| `FIELD` | Dotted field path, e.g. `Redis.Password` |
| `TYPE` | The field's Go type, e.g. `secret.String` |
| `CHAIN` | The `source:` tag's comma-separated ref chain, in precedence order (see [Source chains and precedence](../concepts#source-chains-and-precedence)) |
| `DEFAULT` | The `default:` tag's value, or `-` if none |
| `OPTIONAL` | Whether the field carries `optional:"true"` |
| `SENSITIVE` | Whether the field's Go type is `secret.String`/`secret.Bytes`, or any ref in its chain uses a secret-bearing scheme |

### `mamori schema`

```bash
mamori schema ./... [--type=Name]
```

Emits a JSON Schema (draft 2020-12) derived from each qualifying struct's field types and `validate:` tags - the same struct shape `explain` describes, in a form a JSON Schema validator can consume directly. If exactly one struct qualifies (a `--type` filter, or only one struct in the loaded packages carries a `source:` tag), the output is a single schema document. If more than one qualifies, the output is a JSON array of documents, each carrying a `title` of `package.TypeName`.

`validate:` rules translate as far as JSON Schema's vocabulary allows: `required` and `oneof` map directly; `gte`/`lte` (always numeric) and `min`/`max` (numeric bound on a number field, rune-count length on a string field) map to `minimum`/`maximum` or `minLength`/`maxLength` depending on the field's own JSON type. A `default:` tag becomes the schema's `default`, typed as a JSON number where the field is numeric.

### `mamori policy`

```bash
mamori policy ./... --format=aws-iam|gcp|external-secret
```

Emits a least-privilege access artifact derived entirely from the `source:` refs found in the loaded packages - never resolving anything, and never fabricating an identifier no ref actually carries:

- **`aws-iam`** - an IAM policy document granting `secretsmanager:GetSecretValue` on every `aws-sm://` ref and `ssm:GetParameter`/`ssm:GetParameters` on every `aws-ps://` ref. **The account ID and region are not part of an `aws-sm://`/`aws-ps://` ref** (both come from ambient AWS config at resolve time), so every ARN uses a `*:*` placeholder for them - fill those in before applying the policy.
- **`gcp`** - a document listing `roles/secretmanager.secretAccessor` and the `projects/<project>/secrets/<name>` resource name for every `gcp-sm://` ref. A `gcp-sm://` ref's project is part of the ref itself (`gcp-sm://<project>/<secret>`), so the real project is used in the normal case; `PROJECT` only appears as a placeholder for a malformed ref missing that segment. This is a summary an operator turns into IAM bindings (e.g. one `gcloud secrets add-iam-policy-binding` per resource), not a ready-to-apply GCP IAM policy - a real binding also needs a principal, which no `source:` ref ever carries.
- **`external-secret`** - an `external-secrets.io/v1beta1` `ExternalSecret` manifest with one `spec.data` entry per `aws-sm://`, `aws-ps://`, or `gcp-sm://` ref found. **`spec.secretStoreRef.name` is always a placeholder** (`REPLACE_ME_SECRET_STORE`): no `source:` ref names a Kubernetes `SecretStore`.

A format whose relevant refs are empty still emits a valid, empty artifact, plus a stderr note that nothing was found - never a silent, misleadingly-complete-looking success.

## Live commands

Both live commands GET `/` on a running process's admin endpoint and render the `mamori.Report` that comes back - the same report `w.Status()` returns, described in full on [Observability](../observability). Neither command decodes, validates, or resolves anything itself; they only render.

### `mamori doctor`

```bash
mamori doctor --endpoint <ep> [--json] [--compare ./...]
```

`--json` emits the admin endpoint's raw response body unchanged, byte for byte, instead of a table. `--compare` is the one place a live command also touches source: it statically extracts `source:` refs from the given package patterns (the same walk `explain` uses) and flags any field present in source but missing from the live report, or present live but not found in source - drift between what your code declares and what the running process is actually reporting. `--compare` needs a real, buildable source tree reachable from wherever you run it; it is not a substitute for `explain`, it is a cross-check against it.

### `mamori status`

```bash
mamori status --endpoint <ep> [--watch] [--interval <dur>]
```

Renders the same report table as `doctor`, without `--compare` or `--json`. With `--watch`, it renders immediately and then again on every tick of `--interval` (default `2s`) until interrupted with Ctrl-C, instead of rendering once and exiting.

## Exit codes

Both live commands share one exit-code table, deliberately structured so a script can tell "my config is broken" apart from "I couldn't even see my config":

| Code | Meaning |
| --- | --- |
| `0` | Healthy - the target process reports every field fresh with no terminal error |
| `1` | Unhealthy - reachable, but the target process reports at least one stale or terminally-errored field |
| `2` | Reachable, but not a usable mamori admin API - a 404, or a 200 body that does not decode as a `mamori.Report` (the admin API is off, or this isn't a mamori process at all) |
| `3` | Unreachable - never got an HTTP response at all (connection refused, no such Unix socket, a TLS failure, or a malformed `--endpoint`) |
| `4` | Auth failed - a reachable mamori admin API returned `401` |

A deploy gate or alert can branch on this directly: exit `1` means go fix the config; `2`/`3` mean go fix connectivity (or wire up `mamori.WithAdminHTTP` in the first place); `4` means go fix the credential. Collapsing any of these into a single "not healthy" bit throws away exactly the information an on-call engineer needs first.

`--watch` never returns any of these mid-loop: a single poll's outcome is only ever printed, and the command returns `0` once you interrupt it, since the point of `--watch` is to keep going through a transient failure, not to exit on the first one.

## Endpoint and auth flags

`--endpoint` accepts three forms, matching what `mamori.WithAdminHTTP`/`mamori.Handler` can serve and what the [config server](../server) itself accepts:

- `https://host:port`
- `unix:///path/to.sock`
- `http://host:port` - only with `--insecure` passed explicitly; a plain `http://` endpoint is refused otherwise

Credentials reuse the same `Authenticator` schemes the admin endpoint supports (see [Auth](../auth)):

| Flag | Purpose |
| --- | --- |
| `--bearer` / `--bearer-file` | Bearer token, as a value or a file path (`-` for stdin) |
| `--basic` / `--basic-file` | `user:pass`, as a value or a file path (`-` for stdin) |
| `--client-cert` / `--client-key` | Client certificate and key (PEM) for mTLS |

**Prefer the file/stdin forms.** `--bearer`/`--basic` put the credential directly in `os.Args`, which lands in shell history and is visible to anything that can read this process's argv (e.g. another local user running `ps`). `--bearer-file`/`--basic-file` (including `-` for stdin, so a credential can be piped in without ever touching disk) keep the token out of both. This is the same reasoning [Auth](../auth) gives for preferring a file-backed credential over a bare environment variable or CLI flag wherever one is available.

## CI versus incident: two different tools for the same question

`mamori.Doctor[T]` (the core library function, covered in full on [Observability](../observability#doctor-a-pre-deploy-reachability-check)) and `mamori doctor` (this CLI's live command) answer the same underlying question - "is this config reachable and healthy" - at two different points in a system's life, and neither is a substitute for the other:

- **`mamori.Doctor[T]`** runs *inside* your own build, typically as a build-tagged CI test, before you ever deploy. It exercises your exact provider wiring, middleware, and `Prefix` rewriting, because it *is* your code, calling the same `Load`/`Watch` machinery your service calls. It catches a rotated-away secret or a typo'd ref before it ships.
- **`mamori doctor`** runs from wherever you are, against a process that is *already running*, over the network (or a socket). It has no idea how that process is wired internally; it only knows what the process's own admin endpoint chooses to report. It is what you reach for during an incident, or as a deploy-time smoke check, when the question is "is the thing that's running right now actually healthy," not "would my config be healthy if I deployed it."

Run `mamori.Doctor` in CI to catch a problem before it ships. Run `mamori doctor`/`mamori status` against a live process to find out whether it did.

## See also

[Observability](../observability) covers the `Report`/`FieldStatus` shape and `WithAdminHTTP`/`Handler` in full - what the live commands are clients of. [Config server](../server) covers the separate `server` module, which shares the same endpoint forms and `Authenticator` types as the flags above. [Concepts](../concepts#source-chains-and-precedence) covers source chains and precedence, which is what `explain`'s `CHAIN` column and `policy`'s ref extraction are reading.
