---
layout: ../../../layouts/DocsLayout.astro
title: vet
---

# mamori vet

`mamori vet` is a `go vet` analyzer that catches a specific, easy-to-make mistake: pulling a secret-bearing source into a plain `string` or `[]byte`. It flags any config field wired to a secret backend but typed as a plain value instead of the redacting `secret.String` / `secret.Bytes` types, so the plaintext cannot slip out through logs, `fmt`, or JSON.

It ships inside the same `mamori` binary as the rest of the CLI, so there is nothing extra to install.

## Run it

The analyzer runs two ways from the one binary. Use whichever fits your workflow, both report the same findings and exit non-zero when something is wrong:

```bash
# As a mamori subcommand (patterns default to ./...):
mamori vet ./...

# As a go vet tool:
go vet -vettool=$(which mamori) ./...
```

Given a config struct where one field stores a Vault secret in a plain `string`:

```go
type Config struct {
	Password string        `source:"vault://kv/db#password"` // flagged
	APIKey   secret.String `source:"aws-sm://prod/api-key"`  // ok
}
```

`mamori vet` reports the plain field and points you at the fix:

```text
config.go:2:2: field "Password" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

## What it flags

A field is flagged when both of these are true:

1. It has a `source:"..."` struct tag whose scheme is one of the secret-bearing schemes below.
2. Its Go type is a plain `string` or `[]byte`, not `secret.String` or `secret.Bytes`.

The secret-bearing schemes are the ones that resolve to secret material:

| Scheme       | Backend                     | Always a secret?              |
| ------------ | --------------------------- | ----------------------------- |
| `aws-sm`     | AWS Secrets Manager         | yes                           |
| `gcp-sm`     | Google Cloud Secret Manager | yes                           |
| `azure-kv`   | Azure Key Vault             | yes                           |
| `vault`      | HashiCorp Vault             | yes                           |
| `op`         | 1Password                   | yes                           |
| `sops`       | Mozilla SOPS                | yes                           |
| `doppler`    | Doppler                     | yes                           |
| `k8s-secret` | Kubernetes Secret           | yes                           |
| `aws-ps`     | AWS SSM Parameter Store     | only `SecureString` params    |
| `exec`       | Command output              | mamori marks all of it secret |
| `mamori`     | A mamori config server      | whatever the server marks     |

The last three carry secrets only sometimes, and are flagged anyway. A false positive costs you a `secret.String` on a value that did not need one; a miss costs a leaked credential. For a security check, the cheap mistake is the right one to make.

Config-style schemes (`env`, `file`, `consul`, `k8s-cm`, and the like) never carry secret material, so fields using them are left alone. Fields with no `source` tag, and fields that already use `secret.String` / `secret.Bytes`, are considered correct.

Note `k8s-secret` is listed but `k8s-cm` is not: a ConfigMap is not a secret.

## Covering a custom provider

The built-in list only knows the schemes mamori ships. If you [wrote your own provider](/docs/writing-a-provider/), its scheme is unknown to the analyzer, and a secret of yours sitting in a plain `string` would pass unreported. Add your scheme with `--secret-schemes`:

```bash
mamori vet --secret-schemes=mysecrets ./...
```

Pass several as a comma-separated list. The flag **adds to** the built-in set rather than replacing it, so `aws-sm`, `vault`, and the rest stay covered:

```bash
mamori vet --secret-schemes=mysecrets,corp-kv ./...
```

As a `go vet` tool the flag is namespaced by the analyzer:

```bash
go vet -vettool=$(which mamori) -mamorivet.secret-schemes=mysecrets ./...
```

The value is a bare scheme token, the part before `://`. A full ref such as `mysecrets://prod` is rejected with an error rather than silently matching nothing.

## Chains are checked end to end

A `source` tag may hold a [precedence chain](/docs/concepts/source-chains/) of comma-separated refs rather than a single one, for example `env:TOKEN,vault://kv/token`. `mamori vet` checks every ref in the chain, not just the first, so a sensitive ref anywhere in the chain is flagged even when it sits behind a non-sensitive primary as a lower-priority fallback:

```go
type Config struct {
	Token string `source:"env:TOKEN,vault://kv/token"` // flagged: vault is sensitive, even as the fallback
}
```

```text
config.go:2:2: field "Token" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

The chain is split the same way mamori splits it at runtime, so what the analyzer calls "the refs in this tag" matches what your program actually resolves.

## Fixing a finding

Change the field type to the matching redacting wrapper: `secret.String` for a `string`, `secret.Bytes` for a `[]byte`. Import `github.com/xavidop/mamori/secret` and swap the type; the tag stays the same.

```go
type Config struct {
	Password secret.String `source:"vault://kv/db#password"` // fixed
}
```

The wrapper keeps the plaintext from leaking through `String()`, `fmt`, `encoding/json`, or `log/slog`. See [Secret types](/docs/concepts/secret-types/) for what the wrappers redact.

## In CI

Wire the analyzer into CI so a leaked secret type can never merge. Install the CLI and run it over your packages as a build step, in either mode:

```bash
go install github.com/xavidop/mamori/cmd/mamori@latest
mamori vet ./...
# or, as a go vet tool:
go vet -vettool=$(which mamori) ./...
```

Both modes exit non-zero when a finding is reported, which fails the step.

## How it works

The check is purely structural. `secret.String` and `secret.Bytes` are named struct types, so they never look like a plain `string` or a plain `[]byte`. The analyzer flags a field only when its type matches one of those two plain shapes, which is exactly what tells an unprotected field apart from a redacted one. Because the match is structural, it needs no name matching and does not import the mamori core, so the analyzer stays fast and dependency-light while shipping inside the `mamori` CLI.

The scheme list is declared rather than derived, and it has to be. A provider marks its values secret at resolve time, by returning `Value{Sensitive: true}`, which a static analyzer can never observe: `go vet` does not run your program, and the CLI deliberately imports no provider modules so that no cloud SDK reaches its dependency graph. That is why the built-in set is a fixed list, and why `--secret-schemes` exists for the schemes it cannot know about.

## See also

- [CLI overview](/docs/cli/) - the rest of the `mamori` command set.
- [Concepts: Secret types](/docs/concepts/secret-types/) - the redacting wrappers this analyzer steers you toward.
- [Concepts: Source chains and precedence](/docs/concepts/source-chains/) - how a `source` tag chain is parsed.
