---
layout: ../../layouts/DocsLayout.astro
title: reconcilevet
---

# reconcilevet

`reconcilevet` is a `go vet` analyzer that catches a specific, easy-to-make mistake: storing a secret in a plain `string`.

It flags any struct field whose `source` tag points at a secret-bearing scheme (`aws-sm`, `gcp-sm`, `azure-kv`, `vault`, `op`, `sops`, `k8s-secret`) but whose Go type is a plain `string` or `[]byte` instead of `secret.String` / `secret.Bytes`.

A `source` tag may hold a [precedence chain](../concepts#source-chains-and-precedence) of comma-separated refs rather than a single one, e.g. `env:TOKEN,vault://kv/token`. `reconcilevet` checks every ref in the chain, not just the first, so a sensitive ref anywhere in the chain - even as a lower-priority fallback behind a non-sensitive primary - is still flagged.

## Install and run

```bash
go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
go vet -vettool=$(which reconcilevet) ./...
```

## Example

```go
type Config struct {
	Password string        `source:"vault://kv/db#password"`    // flagged: use secret.String
	APIKey   secret.String `source:"aws-sm://prod/api-key"`      // ok
	LogLevel string        `source:"env:LOG_LEVEL"`              // ok: env is not secret-bearing
	Token    string        `source:"env:TOKEN,vault://kv/token"` // flagged: vault is sensitive, even as the fallback ref
}
```

Running the analyzer on that file reports:

```text
config.go:2:2: field "Password" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
config.go:5:2: field "Token" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

Wire it into CI so a leaked secret type can never merge. The shipped GitHub Actions workflow builds `reconcilevet` and runs it over the module and examples on every push.
