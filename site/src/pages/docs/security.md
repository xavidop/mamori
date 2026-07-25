---
layout: ../../layouts/DocsLayout.astro
title: Security & releases
---

# Security & releases

## Security model

- Sensitive values never pass through `fmt` or logs unredacted. `secret.String` / `secret.Bytes` render as `[REDACTED]` and only `Reveal()` exposes the value.
- `reconcilevet` catches sensitive refs stored in plain `string` / `[]byte` fields at `go vet` time.
- `Zero()` is best-effort and documented as such: Go's GC may already have copied the value, so we make no false promises about memory safety.
- The `exec:` provider is off by default and must be enabled with `WithExecProvider()`. Refs are never interpolated from other resolved values, so there are no injection chains.
- Providers must not log payloads; the conformance kit includes a log-capture assertion.
- `mamori` is a library, not a store: it holds values in process memory only and writes nothing to disk.

### `WithHistory` retains past secrets in memory

`WithHistory(n)` (see [Loading & watching](../usage#retaining-snapshots-with-withhistory)) defaults to `0`: a `Watcher` normally keeps only its current, live snapshot. Turning it on trades that off against operability - being able to inspect what config looked like a few versions back, or `Pin` `Get()` to it - at a cost worth stating plainly:

- Each retained `Snapshot[T]` holds a **full copy of `T`** as it was at that version, including every `secret.String` / `secret.Bytes` field's value at the time.
- A secret that has since rotated is not gone from process memory just because it rotated: it stays reachable through `w.History()[i].Config` (or via `w.Pin` to that version) for as long as that snapshot sits inside the `n`-snapshot window.
- This does not weaken redaction in logs, `fmt`, or JSON - that protection lives on the `secret.String` / `secret.Bytes` type itself, not on whether history is enabled. It only extends how long an old secret value sits somewhere in memory, reachable by any application code that calls `Reveal()` on it.

Enable `WithHistory` deliberately: pick `n` for the operational need you actually have (recent-regression debugging, an audit trail of the last few changes), not "as large as possible." Leave it at the default `0` unless you have a concrete reason to look backward.

### Supply chain

The core module has minimal dependencies; each provider module isolates its SDK blast radius. Releases are published with checksums and SLSA provenance via GoReleaser.

Report vulnerabilities privately via [GitHub Security Advisories](https://github.com/xavidop/mamori/security/advisories/new) - never in a public issue.

## Two HTTP surfaces: metadata now, values later

mamori exposes, or will expose, exactly two things over HTTP, and they sit on opposite sides of a hard line:

- **The admin endpoint** (`Handler`, `WithAdminHTTP` - see [HTTP exposure](../observability#http-exposure)) serves operational metadata only: a `Report` of field paths, provider schemes, refs with sensitive query options redacted, staleness, and error kinds. It **cannot serve a configuration value.** There is no route, under any option, that returns one - the response body is always `w.Status()`, which never carries a resolved value by construction, not by a check the handler could forget to make.
- **The config server**, a later addition, will serve resolved values, gated by an authorization policy expressed in terms of the `Identity` an `Authenticator` returns (see [Auth](../auth)). It does not exist yet - nothing shipped today can hand a caller a secret over HTTP.

Keep the two apart when reasoning about exposure: pointing an unauthenticated admin endpoint at the public internet is a metadata leak, not a secret leak, but it is still a leak worth avoiding, and it stops being true the day the config server ships against the same host.

### `WithAdminHTTP` is unauthenticated by default

`WithAdminHTTP` starts with no `Authenticator` attached: any request that can reach the port gets the `Report`. Do at least one of the following before exposing it beyond a single trusted host:

- **Bind to localhost or a non-ingress port** - `WithAdminHTTP("127.0.0.1:9090")` rather than a port your load balancer or ingress controller forwards - so the endpoint is reachable only from the same host or network namespace.
- **Front it with `WithAuth`**, attaching one of the shipped `Authenticator` schemes or your own (see [Auth](../auth)), so a request must present a credential.
- **Layer `WithAdminTLS`** on top, so a credential sent to the endpoint - a bearer token, a basic-auth password - is never sent in the clear. `WithAdminTLS` has no effect without `WithAdminHTTP`.

These combine: bind to a non-ingress port *and* require auth when the endpoint has to be reachable from more than one host.

### Ref redaction

Every `Report.Fields[i].Ref`, wherever it surfaces - `Status()`, `Doctor`, or the admin HTTP endpoint - has a fixed denylist of query-option names redacted before it can leave the process: `token`, `password`, `secret`, `key`, `apikey`, `api_key`, `sas`, `credential`, `client_secret`, `secret_access_key`, `access_key`, `private_key`, `secret_key`, `pwd`, `passwd` (matched case-insensitively). The scheme, path, key, and any non-sensitive option are left intact so the ref stays useful for diagnostics; only the values behind those names are replaced with `secret.Redacted`. See [Observability](../observability) for the `Report` shape this applies to.

## Out of scope

`mamori` is not a secrets store and provides no encryption at rest. Its only server component is the optional, metadata-only admin HTTP endpoint described above - it exposes no way to serve a configuration value. Protecting the backends it reads from (IAM policies, Vault ACLs, KMS keys) is your infrastructure's responsibility.

## Releases and versioning

Core releases are automated from [Conventional Commits](https://www.conventionalcommits.org/). When commits land on `main`, **semantic-release** decides the next version (`fix:` -> patch, `feat:` -> minor, breaking -> major), updates the changelog, and creates the `vX.Y.Z` tag; **GoReleaser** then builds the `reconcilevet` binary and publishes the GitHub Release with checksums, an SBOM, and SLSA provenance.

Modules are versioned with semantic-version git tags. The core module tags as `v0.1.0`; each submodule tags with its path prefix:

```text
v0.1.0                      # core
providers/aws/v0.1.0        # AWS provider module
x/otel/v0.1.0               # OpenTelemetry bridge
```

Import a specific version the usual way:

```bash
go get github.com/xavidop/mamori@v0.1.0
go get github.com/xavidop/mamori/providers/aws@v0.1.0
```

Each provider module keeps its own release cadence, so a breaking change in one SDK never forces a core release.
