---
layout: ../../../layouts/DocsLayout.astro
title: Deploy and expose
---

# Deploy and expose

A server binds one or more listeners and serves the same v1 wire protocol on all of them. Declare a listener with `Unix` or `TCP`; call either any number of times. `Serve` binds every `Unix` listener before every `TCP` listener, then blocks.

```go
func Unix(path string, mode os.FileMode) Option
func TCP(addr string, opts ...TCPOption) Option
func TLS(cfg *tls.Config) TCPOption
func InsecureNoTLS() TCPOption
```

## Expose over a Unix socket

Bind a `0600` socket (owner read/write only). Every connection here has its kernel-verified peer credentials captured, so a [`PeerCred`](/docs/auth/schemes/#peercred) authenticator can identify callers by uid/gid.

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	"github.com/xavidop/mamori/server"
)

func main() {
	srv, err := server.New(
		server.Bind("db-password", "vault://secret/data/db#password"),
		server.WithProvider(vaultProvider),
		server.WithPolicy(server.AllowAll()),
		server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),
		server.Unix("/run/mamori/server.sock", 0600),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	if err := srv.Serve(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

`Serve` unlinks any stale file at the path before binding and removes it again on `Close`, so a crashed server always restarts cleanly.

## Expose over TLS TCP

`TCP` without TLS fails construction. Pass `TLS(cfg)` for the ordinary case:

```go
package main

import (
	"context"
	"crypto/tls"
	"log"
	"os"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	"github.com/xavidop/mamori/server"
)

func main() {
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	srv, err := server.New(
		server.Bind("db-password", "vault://secret/data/db#password"),
		server.WithProvider(vaultProvider),
		server.WithPolicy(server.AllowAll()),
		server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),
		server.TCP(":8443", server.TLS(tlsConfig)),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	if err := srv.Serve(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

`InsecureNoTLS()` is the only way to serve plaintext TCP. Its name is deliberately greppable: `grep -r InsecureNoTLS` finds every place bindings are served without TLS, which should never be an accident.

```go
server.TCP(":8080", server.InsecureNoTLS()) // deliberate, greppable opt-out
```

`NoAuth()` is refused on any TCP listener; over TCP, authentication is mandatory. `Addrs() []net.Addr` reports every bound listener's address, useful for discovering the OS-chosen port after binding to `:0`.

## Both at once

Call `Unix` and `TCP` together. Both run under the same policy and wire handler; `Serve` binds every listener before accepting on any.

```go
srv, err := server.New(
	server.Bind("db-password", "vault://secret/data/db#password"),
	server.WithProvider(vaultProvider),
	server.WithPolicy(server.AllowAll()),
	server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),
	server.Unix("/run/mamori/server.sock", 0600),
	server.TCP(":8443", server.TLS(tlsConfig)),
)
```

## Audit requests

```go
func WithAudit(l *slog.Logger) Option
```

Audit logging is off by default. It is a diagnostic aid, not the enforcement mechanism (`Policy` and `Authenticator` decide access). When configured, one structured record is written per request (per name, for a batch or watch subscription), carrying identity subject, binding name, allow/deny decision, resulting `kind`, HTTP status, and latency.

```go
srv, err := server.New(
	// ... bindings, policy, auth, transport ...
	server.WithAudit(slog.New(slog.NewJSONHandler(os.Stdout, nil))),
)
```

**The audit record structurally cannot carry a resolved value.** The Go type backing it has no field a value's bytes could be assigned to, so a `mamori.Value` can never be wired into the log.

## Next

- [Server auth and policy](/docs/server/authorization/) - authenticate and authorize callers.
- [Server wire protocol](/docs/server/wire-protocol/) - the routes served on every listener.
- [Config server overview](/docs/server/) - the fan-out model and blast radius.
