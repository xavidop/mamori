---
layout: ../../../layouts/DocsLayout.astro
title: Server bindings
---

# Server bindings

A binding maps a name (what a client requests) to a ref (what actually gets resolved). Bindings are the whole security model: a client sends a name, never a ref, so a request can never carry `file:///etc/shadow` or a client-chosen `exec:` command.

## Declare bindings

`Bind` declares one binding inline; `BindFile` reads them from YAML.

```go
func Bind(name, ref string) Option
func BindFile(path string) Option
```

```go
srv, err := server.New(
	server.Bind("db-password", "vault://secret/data/db#password"),
	server.Bind("api-key", "aws-sm://prod/api-key"),
	server.BindFile("/etc/mamori/bindings.yaml"),
	// ... policy, auth, transport ...
)
```

```yaml
# /etc/mamori/bindings.yaml
bindings:
  db-password: vault://secret/data/db#password
  api-key: aws-sm://prod/api-key
```

`Bind`/`BindFile` are the only way a binding comes into existence, and the operator calls them in the server's startup code. A duplicate binding name fails `New`. All declarations are validated together in one pass after every option applies, so declaration order never matters.

## Register providers

Each binding resolves through the provider registered for its ref's scheme.

```go
func WithProvider(p mamori.Provider) Option
```

`WithProvider` registers a `Provider` for its own `Scheme()`. Every provider a binding can use is one the operator named explicitly; there is no process-wide registry fallback. A binding whose scheme has no registered provider still constructs cleanly and simply reports a resolve error on lookup, while every other binding keeps working.

## Allow `exec:` and `mamori:` schemes

Two ref schemes are rejected at construction unless explicitly allowed:

```go
func AllowExec() Option
func AllowChaining() Option
```

```go
srv, err := server.New(
	server.Bind("build-token", "exec://vault-agent-token"),
	server.AllowExec(),
	server.WithProvider(myExecProvider), // required: core ships no exec provider constructor
	// ... policy, auth, transport ...
)
```

- **`exec:`** runs an arbitrary command on the server's host. `AllowExec()` lifts the construction-time gate, but core's `exec:` provider has no exported constructor, so making an `exec:` binding resolve also requires you to supply your own exec `Provider` via `WithProvider`. Two deliberate steps for a scheme that means remote command execution reachable by every authorized consumer.
- **`mamori:`** chains to another config server. `AllowChaining()` lifts the gate; without it, a `mamori:` binding could quietly wire up a cycle.

Every other scheme (`env:`, `aws-sm:`, `vault:`, `file:`, `gcp-sm:`, and so on) needs neither option.

## Next

- [Server auth and policy](/docs/server/authorization/) - decide who may request which binding.
- [Deploy and expose](/docs/server/transports/) - serve the bindings over a socket or TLS TCP.
- [Config server overview](/docs/server/) - the fan-out model and blast radius.
