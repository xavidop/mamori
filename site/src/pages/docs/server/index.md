---
layout: ../../../layouts/DocsLayout.astro
title: Config server
---

# Config server

`server` (import path `github.com/xavidop/mamori/server`) is a standalone process that fronts a fixed, operator-declared table of name-to-ref bindings and serves the resolved values to authenticated, authorized callers over a small v1 HTTP wire protocol (Unix socket and TLS TCP). Run it as a sidecar or a fan-out: one process holds the backend credentials and watches each upstream once, everyone else asks it for values by name. That concentration is also its main cost, so read [Blast radius](#blast-radius) before you deploy one.

```mermaid
flowchart LR
  AWS[AWS Secrets Manager] --> S
  Vault[Vault] --> S
  GCP[GCP Secret Manager] --> S
  S["mamori config server (holds every credential, one watch per binding)"]
  S -->|"mamori:// over UDS or TLS"| A[Service A]
  S -->|mamori://| B[Service B]
  S -->|mamori://| C[Service C]
```

## Quick start

Stand up a server that fronts two bindings over a `0600` Unix socket, gated by a bearer token and a per-subject policy:

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
	ctx := context.Background()

	srv, err := server.New(
		// Operator-declared bindings only: a client requests a name, never a ref.
		server.Bind("db-password", "vault://secret/data/db#password"),
		server.Bind("api-key", "aws-sm://prod/api-key"),

		// Providers resolve the ref schemes above (no registry fallback here).
		server.WithProvider(vaultProvider),
		server.WithProvider(awsProvider),

		// Authorization and authentication are both mandatory.
		server.WithPolicy(server.PrefixPolicy(map[string][]string{
			"svc-orders": {"db-password"},
		})),
		server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN")))),

		// A Unix socket, owner read/write only.
		server.Unix("/run/mamori/server.sock", 0600),
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

`New` starts nothing: it validates every security invariant (a `Policy` is configured, auth is configured or `NoAuth()` is explicit, `NoAuth()` is never combined with TCP, every binding parses and passes its scheme gate) and returns a `*Server`. `Serve(ctx)` begins upstream watching, binds every transport, and blocks until a listener fails or the server is closed. `Close()` is idempotent and safe even if `Serve` never ran, so defer it unconditionally right after `New`.

`Close()` releases only what the server itself created: its listeners, its upstream watch goroutines, and its contexts. A provider passed in through `server.WithProvider` is **not** one of them. It belongs to whoever constructed it, exactly as under `mamori.WithProvider`, and the server never asserts it for `io.Closer` and never closes it. That is worth spelling out here more than anywhere else, because this is a long-lived operator process fanning out to backends, so it is a likely place to be holding a provider with a real connection behind it (`postgres`, `redis`, `mongodb`, `etcd`, ...). Close those yourself, after `srv.Close()`. See [Who closes a provider](/docs/writing-a-provider/#who-closes-a-provider).

A client resolves a binding through the `mamori://` provider ([mamoriprov](/docs/providers/mamori/)):

```go
type Config struct {
	DBPassword secret.String `source:"mamori://db-password"`
}

cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(mamoriprov.New(
		mamoriprov.Config{Endpoint: "unix:///run/mamori/server.sock"},
		mamoriprov.WithHeader("Authorization", "Bearer "+os.Getenv("SERVER_TOKEN")),
	)),
)
```

Or read it straight off the wire:

```sh
curl --unix-socket /run/mamori/server.sock \
  -H "Authorization: Bearer $SERVER_TOKEN" \
  http://unix/v1/values/db-password
```

## Refresh re-reads the server, not the upstream

A client watching a `mamori://`-resolved field can call `w.Refresh(ctx)` on its own `Watcher` at any time (see [Rotation safety](/docs/usage/refresh/)). For that field, `Refresh` re-reads whatever value this server currently holds - a fresh round trip over `mamori://`, past any client-side cache - which is genuinely useful right after a dropped stream. **It does not reach past this server to make it re-resolve its own upstream.** The server already watches its upstream continuously, on its own schedule, regardless of any client's `Refresh`; a client re-reading it is just another read of a value that is already as fresh as that watch keeps it.

Upstream propagation - a client's `Refresh` forcing this server to go fetch a fresh value from Vault, AWS, or GCP right now, ahead of its own watch - is a real, wanted feature, not a rejected one. It is planned, here in the config server rather than on the [admin endpoint](/docs/observability/admin/#no-post-refresh), and it will be **`Policy`-gated** when it ships: available to callers a policy explicitly grants it to, not to every caller merely authorized to read a binding's value. It has its own spec still to come; there is no route, verb, or `Policy` surface for it today.

The gate has to exist because of what this server is *for*. Its entire value proposition, stated at the top of this page, is that N consumers cost one upstream watch rather than N. An ungated client-triggered upstream refresh inverts exactly that: N clients across M bindings would turn into N×M on-demand calls against backends that are typically rate-limited and billed per call - and every one of those N callers would need nothing more than the read authorization it already has to trigger all of them. Gating this behind `Policy`, once it ships, is what keeps "may read a binding's value" and "may force an upstream round trip on every reader's behalf" from collapsing into the same permission.

## Blast radius

**A config server is deliberately the highest-blast-radius piece of mamori.** A single `Load` or `Watch` caller holds only the credentials it was given; a config server, by design, concentrates every backend credential its bindings touch into one process, reachable by every consumer it serves. Compromising it is strictly worse than compromising any one consumer.

The mitigations are structural, not conventions an operator has to remember:

- **No client-supplied refs.** A client can only name a binding, never supply a ref, so `file:///etc/shadow` and arbitrary `exec:` commands are unreachable by construction. See [Server bindings](/docs/server/bindings/).
- **Mandatory `Policy`.** `New` will not construct a `Server` with no authorization policy, not even a deny-all default. See [Server auth and policy](/docs/server/authorization/).
- **Mandatory authentication over the network.** `NoAuth()` is refused with a TCP listener; the only unauthenticated posture is Unix-socket-only.
- **Mandatory TLS over TCP.** Plaintext TCP requires the deliberately uncomfortable, greppable `InsecureNoTLS()`. See [Deploy and expose](/docs/server/transports/).
- **No values in the audit trail.** Structurally impossible, not merely discouraged.

These mitigations narrow how the concentration can be abused, not the concentration itself. A trusted sidecar reading a handful of bindings over a `0600` Unix socket is a very different risk from a network-reachable server fronting every credential a fleet needs. Treat the config server's own host and process the way you would treat a secrets-manager credential itself.

## Next

- [Server bindings](/docs/server/bindings/) - `Bind`/`BindFile`, the operator-declared-only model, and the `exec:`/`mamori:` gates.
- [Deploy and expose](/docs/server/transports/) - Unix sockets, TLS TCP, running both, and the audit log.
- [Server auth and policy](/docs/server/authorization/) - `WithAuth`, the shipped schemes, and the `Policy` model.
- [High availability](/docs/server/ha/) - running several replicas: readiness gating, draining, freshness, and client failover.
- [Server wire protocol](/docs/server/wire-protocol/) - v1 routes, response shapes, the fresh-vs-stale `kind`, watch (SSE), and healthz.

## See also

[Auth](/docs/auth/) covers every shipped `Authenticator`, including `PeerCred`. [Providers: mamori](/docs/providers/mamori/) is the `mamori://` client that resolves bindings against this server. [Security](/docs/security/) covers the blast-radius point in the context of both HTTP surfaces. [Observability](/docs/observability/) covers `WithAdminHTTP`, the metadata-only sibling that answers "is my config healthy" rather than "give me a value." [Rotation safety](/docs/usage/rotation/) covers `w.Refresh` itself, including what it means for a field resolved through this server.
