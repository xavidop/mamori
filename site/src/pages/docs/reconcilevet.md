---
layout: ../../layouts/DocsLayout.astro
title: reconcilevet
---

# reconcilevet

`reconcilevet` is a `go vet` analyzer that catches a specific, easy-to-make mistake: pulling a secret-bearing source into a plain `string` or `[]byte`. It flags any config field wired to a secret backend but typed as a plain value instead of the redacting `secret.String` / `secret.Bytes` types, so the plaintext cannot slip out through logs, `fmt`, or JSON.

## Install and run

Install the standalone driver, then run it as a `go vet` tool over your module:

```bash
go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
go vet -vettool=$(which reconcilevet) ./...
```

Given a config struct where one field stores a Vault secret in a plain `string`:

```go
type Config struct {
	Password string        `source:"vault://kv/db#password"` // flagged
	APIKey   secret.String `source:"aws-sm://prod/api-key"`  // ok
}
```

`go vet` reports the plain field and points you at the fix:

```text
config.go:2:2: field "Password" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

## What it flags

A field is flagged when both of these are true:

1. It has a `source:"..."` struct tag whose scheme is one of the secret-bearing schemes below.
2. Its Go type is a plain `string` or `[]byte`, not `secret.String` or `secret.Bytes`.

The secret-bearing schemes are the ones that resolve to secret material:

| Scheme       | Backend                     |
| ------------ | --------------------------- |
| `aws-sm`     | AWS Secrets Manager         |
| `gcp-sm`     | Google Cloud Secret Manager |
| `azure-kv`   | Azure Key Vault             |
| `vault`      | HashiCorp Vault             |
| `op`         | 1Password                   |
| `sops`       | Mozilla SOPS                |
| `k8s-secret` | Kubernetes Secret           |

Config-style schemes (`env`, `file`, `consul`, `exec`, and the like) never carry secret material, so fields using them are left alone. Fields with no `source` tag, and fields that already use `secret.String` / `secret.Bytes`, are considered correct.

## Chain-aware analysis

A `source` tag may hold a [precedence chain](../concepts#source-chains-and-precedence) of comma-separated refs rather than a single one, for example `env:TOKEN,vault://kv/token`. `reconcilevet` checks every ref in the chain, not just the first, so a sensitive ref anywhere in the chain is flagged even when it sits behind a non-sensitive primary as a lower-priority fallback:

```go
type Config struct {
	Token string `source:"env:TOKEN,vault://kv/token"` // flagged: vault is sensitive, even as the fallback
}
```

```text
config.go:2:2: field "Token" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

The chain is split the same way mamori splits it at runtime, so what the analyzer calls "the refs in this tag" matches what your program actually resolves.

## How to fix a finding

Change the field type to the matching redacting wrapper: `secret.String` for a `string`, `secret.Bytes` for a `[]byte`. Import `github.com/xavidop/mamori/secret` and swap the type; the tag stays the same.

```go
type Config struct {
	Password secret.String `source:"vault://kv/db#password"` // fixed
}
```

The wrapper keeps the plaintext from leaking through `String()`, `fmt`, `encoding/json`, or `log/slog`. See [Secret types](../concepts#secret-types) for what the wrappers redact.

## Integrating into CI

Wire the analyzer into CI so a leaked secret type can never merge. Install the driver and run it over your packages as a build step:

```bash
go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
go vet -vettool=$(which reconcilevet) ./...
```

`go vet` exits non-zero when it reports a finding, which fails the step. The mamori repo ships a GitHub Actions workflow that builds `reconcilevet` and runs it over the module and examples on every push.

## How it works

`reconcilevet` distinguishes good fields from bad ones structurally. `secret.String` and `secret.Bytes` are named struct types, so they never look like a plain `string` (a `*types.Basic` of string kind) or a plain `[]byte` (a `*types.Slice` of byte elements). The analyzer flags a field only when its type matches one of those two plain shapes, which is exactly what tells a redacted field apart from an unprotected one. Because the check is purely structural, no name matching or import of the mamori core is required.

That structural approach is what lets `reconcilevet` stay a standalone module. It lives two levels deep in the repo and is not part of the root `go.work`, and it does not import the mamori core at runtime. Its only shared dependency is the `sourcetag` helper, which replicates mamori's chain-split and scheme rules (the text before the first `:` is the scheme) closely enough that the analyzer and the runtime agree on what the refs in a tag mean, without either depending on the other. Run its own checks with the workspace disabled:

```bash
cd tools/reconcilevet
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

## See also

- [Concepts: Secret types](../concepts#secret-types) for the redacting wrappers this analyzer steers you toward.
- [Concepts: Source chains and precedence](../concepts#source-chains-and-precedence) for how a `source` tag chain is parsed.
