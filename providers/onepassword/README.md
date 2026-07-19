# mamori 1Password provider

[1Password Connect](https://developer.1password.com/docs/connect/) provider for [mamori](https://github.com/xavidop/mamori). Pure `net/http` against the Connect REST API - no third-party SDK.

```bash
go get github.com/xavidop/mamori/providers/onepassword
```

```go
import _ "github.com/xavidop/mamori/providers/onepassword" // registers op://
```

## Scheme

```
op://<vault>/<item>/<field>
```

```go
type Config struct {
    DBPassword secret.String `source:"op://Production/postgres/password"`
}
```

- Three path segments: vault, item, and field (by label or id).
- Values are marked `Sensitive`. `Value.Version` is the item version (or a content hash).

## Authentication

A 1Password Connect host and token:

```bash
export OP_CONNECT_HOST="https://connect.example:8080"
export OP_CONNECT_TOKEN="eyJ..."
```

or explicitly:

```go
mamori.WithProvider(onepassword.New(
    onepassword.WithHost("https://connect:8080"),
    onepassword.WithToken("eyJ..."),
))
```

## Watch

No native change notification - mamori polls (interval + jitter).

## What is verified

- ✅ Unit tests and the [`providertest`](../../providertest) conformance kit run against an in-process HTTP fake of the Connect API (injected `*http.Client`).
- ⚠️ Live Connect behavior is exercised by `//go:build integration` tests requiring a running Connect server, **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
