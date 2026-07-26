# mamori config server (`server`)

`github.com/xavidop/mamori/server` is a separate module: a standalone process built around mamori's resolution machinery that fronts a fixed, operator-declared table of name-to-ref bindings and serves the resolved values to authenticated, authorized callers over Unix sockets and TLS TCP.

**Read this before deploying one.** This is deliberately the highest-blast-radius component in the mamori project: it concentrates every backend credential its bindings touch into one process, reachable by every consumer it serves. The full accounting of that tradeoff, and the structural mitigations that narrow it (no client-supplied refs, mandatory authorization policy, mandatory auth and TLS over the network, no values in the audit log), is on the [Config server](https://mamorigo.dev/docs/server#blast-radius) docs page. This README is a quick-start, not the full picture.

```sh
go get github.com/xavidop/mamori/server
```

## Minimal usage

```go
package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	"github.com/xavidop/mamori/server"
)

func main() {
	ctx := context.Background()

	srv, err := server.New(
		// Operator-declared bindings only: a client can request a name, never a ref.
		server.Bind("db-password", "vault://secret/data/db#password"),
		server.Bind("api-key", "aws-sm://prod/api-key"),

		// Providers resolve the ref schemes above; there is no registry fallback here.
		server.WithProvider(vaultProvider),
		server.WithProvider(awsProvider),

		// Authorization is mandatory - AllowAll() is the explicit "trust everyone" choice.
		server.WithPolicy(server.PrefixPolicy(map[string][]string{
			"svc-orders": {"db-password"},
		})),

		// Authentication is mandatory unless NoAuth() is used, and NoAuth() is
		// refused outright on a TCP listener.
		server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),

		// A Unix socket, 0600, plus a TLS TCP listener under the same policy.
		server.Unix("/run/mamori/server.sock", 0600),

		server.WithAudit(slog.Default()), // identity/name/decision/kind/latency - never the value bytes
	)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	if err := srv.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}
```

A client reads a value over the v1 wire protocol:

```sh
curl --unix-socket /run/mamori/server.sock \
  -H "Authorization: Bearer $SERVER_TOKEN" \
  http://unix/v1/values/db-password
```

## What this module is (and is not)

- It is a **standalone deployment**, not a library your application imports to resolve its own config - that is core's `Load`/`Watch` (see the [root README](../README.md)). Run it as its own process; other processes talk to it over the wire protocol.
- Bindings are **fixed and operator-declared**. `server.Bind`/`server.BindFile` are the only way a binding is created; a client names a binding, never a ref, so it can never make this process resolve `file:///etc/shadow` or a ref of its own choosing.
- `exec:` and `mamori:` (chaining) refs are rejected at construction unless the operator explicitly opts in with `server.AllowExec()` / `server.AllowChaining()`.
- A `Policy` (authorization) and an `Authenticator`-or-`NoAuth()` (authentication) decision are both **mandatory**; `server.New` refuses to construct a `*Server` without them.
- The audit log (`server.WithAudit`), when enabled, records identity, binding name, allow/deny decision, error kind, and latency - structurally never the resolved value's bytes.

## Documentation

- 📖 **Full docs, including the wire protocol reference and the blast-radius discussion:** https://mamorigo.dev/docs/server
- 🔑 **Authentication schemes** (`BasicAuth`, `BearerToken`, `APIKey`, `MTLS`, `PeerCred`, `AnyOf`/`AllOf`): https://mamorigo.dev/docs/auth
- 📦 **API reference:** https://pkg.go.dev/github.com/xavidop/mamori/server

## Development

This module lives one level below the repo root and depends on the core `github.com/xavidop/mamori` module. Run the usual commands from within `server/`:

```sh
cd server
go build ./...
go vet ./...
go test ./...
```
