---
layout: ../../layouts/DocsLayout.astro
title: reconcilevet
---

# reconcilevet

`reconcilevet` is a `go vet` analyzer that catches a specific, easy-to-make mistake: storing a secret in a plain `string`.

It flags any struct field whose `source` tag points at a secret-bearing scheme (`aws-sm`, `gcp-sm`, `azure-kv`, `vault`, `op`, `sops`, `k8s-secret`) but whose Go type is a plain `string` or `[]byte` instead of `secret.String` / `secret.Bytes`.

## Install and run

```bash
go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
go vet -vettool=$(which reconcilevet) ./...
```

## Example

```go
type Config struct {
	Password string        `source:"vault://kv/db#password"` // flagged: use secret.String
	APIKey   secret.String `source:"aws-sm://prod/api-key"`   // ok
	LogLevel string        `source:"env:LOG_LEVEL"`           // ok: env is not secret-bearing
}
```

Running the analyzer on that file reports:

```text
config.go:2:2: field "Password" carries a secret-bearing source (vault://kv/db#password)
  but is a plain string; use secret.String to keep it out of logs
```

Wire it into CI so a leaked secret type can never merge. The shipped GitHub Actions workflow builds `reconcilevet` and runs it over the module and examples on every push.
