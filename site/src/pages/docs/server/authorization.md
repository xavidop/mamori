---
layout: ../../../layouts/DocsLayout.astro
title: Server auth and policy
---

# Server auth and policy

Every request runs two gates in order: **authenticate** (who is this caller) then **authorize** (may this caller have this name). Both are mandatory; `New` refuses to construct a `Server` that is missing either.

## Authenticate callers

```go
func WithAuth(a mamori.Authenticator) Option
func NoAuth() Option
```

The config server reuses core's `Authenticator`/`Identity` unchanged, so [every shipped scheme](/docs/auth/schemes/) works here exactly as it does against the admin HTTP endpoint: `BasicAuth`, `BearerToken`, `APIKey`, `MTLS`, `PeerCred`, and their `AnyOf`/`AllOf` composition.

```go
server.WithAuth(mamori.BearerToken(secret.NewString(os.Getenv("SERVER_TOKEN"))))
```

`NoAuth()` exists for a Unix-socket-only deployment where the filesystem (and, with `PeerCred`, the kernel) already bounds who can connect. **`NoAuth()` is refused on a TCP listener**, and `New` (not `Serve`) rejects that combination, so the mistake surfaces before any port is bound.

### Authenticate a Unix peer with `PeerCred`

[`PeerCred`](/docs/auth/schemes/#peercred) is the strongest option for a Unix-socket deployment: it authenticates a connecting process by the uid/gid the kernel reports at accept time (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on Darwin), which the client cannot spoof. It requires this server's `Unix(...)` transport, which wires up the plumbing that reads a connection's peer credentials.

```go
srv, err := server.New(
	server.Bind("db-password", "vault://secret/data/db#password"),
	server.WithProvider(vaultProvider),
	server.WithPolicy(server.PrefixPolicy(map[string][]string{
		"uid:1000": {"db-password"}, // Identity.Subject is "uid:<peer uid>"
	})),
	server.WithAuth(mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{1000}})),
	server.Unix("/run/mamori/server.sock", 0600),
)
```

## Authorize with a Policy

```go
type Policy interface {
	Allow(id mamori.Identity, name string) error
}

type PolicyFunc func(id mamori.Identity, name string) error

func AllowAll() Policy
func PrefixPolicy(rules map[string][]string) Policy
```

`WithPolicy(p Policy)` is mandatory: `New` refuses to construct a `Server` with no policy, not even a deny-all default.

- **`AllowAll()`** permits every identity access to every binding. Use it for a trusted-sidecar deployment where per-name rules add configuration without adding security.
- **`PrefixPolicy(rules)`** grants access by subject: `rules[id.Subject]` is a list of `path.Match` globs checked against the requested binding name (`*` and `?` do not cross a `/`; `[...]`/`[^...]` character classes work as in a shell glob). A subject with no entry is denied, with no fallback default-allow.
- **`PolicyFunc`** adapts a plain function, the same pattern as `mamori.AuthFunc`.

```go
server.WithPolicy(server.PrefixPolicy(map[string][]string{
	"svc-orders":  {"db-*", "api-key"},
	"svc-billing": {"stripe-*"},
}))
```

```go
server.WithPolicy(server.PolicyFunc(func(id mamori.Identity, name string) error {
	if strings.HasPrefix(name, id.Subject+"-") {
		return nil
	}
	return server.ErrDenied
}))
```

A denied name is indistinguishable from a nonexistent one: `Policy.Allow` is handed only an `Identity` and a name (never the binding table), and the wire handler always answers a denial with the same `403 permission_denied`. Authorization runs before the binding table is consulted, so a policy can never enumerate what a server serves. Recognize a denial with `errors.Is(err, server.ErrDenied)`.

## Next

- [Server bindings](/docs/server/bindings/) - the names a policy grants access to.
- [Server wire protocol](/docs/server/wire-protocol/) - where the auth-then-authorize order runs on every request.
- [Auth schemes](/docs/auth/schemes/) - every shipped `Authenticator` in full.
