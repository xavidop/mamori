---
layout: ../../../layouts/DocsLayout.astro
title: 1Password provider
---

# 1Password

[1Password Connect](https://developer.1password.com/docs/connect/) over the REST API. Pure `net/http`, no third-party SDK.

| | |
| --- | --- |
| Scheme | `op://` |
| Module | `github.com/xavidop/mamori/providers/onepassword` |
| Sensitive | yes |
| Watch | poll |
| Auth | `OP_CONNECT_HOST`, `OP_CONNECT_TOKEN` |

## Install

```bash
go get github.com/xavidop/mamori/providers/onepassword
```

```go
import _ "github.com/xavidop/mamori/providers/onepassword"
```

## Using the ref

An `op://` ref points at one field of one item in a 1Password vault. This matches the familiar 1Password secret-reference format.

```text
op://<vault>/<item>/<field>
```

| Part | Required | What it means |
| --- | --- | --- |
| `<vault>` | yes | Vault name or id. A name is looked up first, then falls back to being treated as an id. |
| `<item>` | yes | Item title or id within that vault. |
| `<field>` | yes | Field label or id on that item. |

**Examples**

- `op://Production/postgres/password` reads the `password` field of the `postgres` item in the `Production` vault.
- `op://Production/stripe/api_key` reads the `api_key` field of the `stripe` item.

```go
type Config struct {
	DBPassword secret.String `source:"op://Production/postgres/password"`
	APIKey     secret.String `source:"op://Production/stripe/api_key"`
}
```

Values are marked `Sensitive`, and `Value.Version` is the item version (or a content hash when the item has no version).

## Explicit configuration

```go
import opprov "github.com/xavidop/mamori/providers/onepassword"

mamori.WithProvider(opprov.New(
	opprov.WithHost("https://connect.internal:8080"),
	opprov.WithToken(os.Getenv("OP_CONNECT_TOKEN")),
))
```

## Watch

1Password Connect has no push channel, so mamori polls (`WithPollInterval` + jitter).

Verified by unit tests and the conformance kit against an in-process HTTP fake of the Connect API (injected `*http.Client`). Live behavior against a running Connect server is covered by `//go:build integration` tests.
