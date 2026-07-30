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

## `?decode=` is rejected on a binding

A binding ref may not carry [`?decode=`](/docs/concepts/decoding/). `New` fails at construction:

```yaml
# rejected: "?decode= is applied by the consuming client, not by this server"
bindings:
  tls-key: aws-sm://prod/tls#key?decode=base64
```

The server resolves a binding through mamori's watch machinery and serves the provider's bytes as they arrive; it never runs the decode pipeline. Accepting the option would mean serving still-base64 bytes to every client with no error anywhere - a wrong value rather than a failure, and one the client cannot detect, because what it receives is a plausible payload. Rejecting is the loud version of the same outcome.

Put the option on the **client's** ref instead, where core applies it:

```go
// server: the binding carries no ?decode=
server.Bind("tls-key", "aws-sm://prod/tls#key")

// client: its own ref does, and core decodes before the field is populated
type Config struct {
	TLSKey secret.Bytes `source:"mamori://tls-key?decode=base64"`
}
```

That split is the intended one: this server is a read-through fan-out of upstream refs, so what it caches and replays is what the upstream holds, while decoding is a property of what a consuming application wants to do with those bytes.

`#key` fragments, **including RFC 6901 pointers**, are unaffected and do work on a binding: providers apply those inside their own `Resolve` via `mamori.SelectKey`, so `aws-sm://prod/db#/credentials/user` binds and resolves normally. Only `?decode=` is refused - which is exactly why it is refused rather than ignored, since one half of the fragment/option grammar working server-side is otherwise a reasonable thing to assume about the other.

## Next

- [Server auth and policy](/docs/server/authorization/) - decide who may request which binding.
- [Deploy and expose](/docs/server/transports/) - serve the bindings over a socket or TLS TCP.
- [Config server overview](/docs/server/) - the fan-out model and blast radius.
