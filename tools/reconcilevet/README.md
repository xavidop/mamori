# reconcilevet

A `go vet` analyzer for the [mamori](https://github.com/xavidop/mamori) library.

## What it flags

`reconcilevet` reports any struct field that binds a **secret-bearing source** to
a **plain, unprotected Go type**. Concretely, a field is flagged when **both** are
true:

1. It has a `source:"..."` struct tag whose scheme is one of the secret-bearing
   schemes:

   | Scheme        | Backend                     |
   | ------------- | --------------------------- |
   | `aws-sm`      | AWS Secrets Manager         |
   | `gcp-sm`      | Google Cloud Secret Manager |
   | `azure-kv`    | Azure Key Vault             |
   | `vault`       | HashiCorp Vault             |
   | `op`          | 1Password                   |
   | `sops`        | Mozilla SOPS                |
   | `k8s-secret`  | Kubernetes Secret           |

   The scheme is parsed the same way mamori does it: the text before the first
   `:` in the tag value (see `ref.go` / `ParseRef`).

2. Its Go type is a plain `string` or `[]byte` - **not**
   `github.com/xavidop/mamori/secret.String` or `secret.Bytes`.

The fix is to store the sensitive value in a redacting wrapper type, which keeps
the plaintext from leaking through `String()`, `fmt`, `encoding/json`, or
`log/slog`.

Config-style schemes (`env`, `file`, `consul`, `exec`, ...) and fields without a
`source` tag are never flagged, and fields that already use `secret.String` /
`secret.Bytes` are considered correct.

## Install & run

```sh
go install github.com/xavidop/mamori/tools/reconcilevet/cmd/reconcilevet@latest
go vet -vettool=$(which reconcilevet) ./...
```

## Example diagnostic

Given:

```go
type Config struct {
    APIKey     secret.String `source:"aws-sm://prod/api#key"`   // OK
    DBPassword string        `source:"aws-sm://prod/db#password"` // flagged
}
```

`reconcilevet` reports:

```
config.go:3:5: field "DBPassword" has a secret-bearing source scheme "aws-sm" but stores it in a plain string; use secret.String or secret.Bytes to keep the value redacted
```

## Development

This module lives two levels deep in the mamori repo and is not part of the root
`go.work`. Run all commands with the workspace disabled:

```sh
cd tools/reconcilevet
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

The analyzer matches the secret wrapper types structurally (they are named struct
types, distinct from plain `string`/`[]byte`), so it does **not** import the
mamori core at runtime. Tests use `analysistest` against fixtures in
`testdata/src/a`, with a minimal stub of the `secret` package under
`testdata/src/github.com/xavidop/mamori/secret`.
