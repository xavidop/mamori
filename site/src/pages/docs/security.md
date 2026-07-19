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

### Supply chain

The core module has minimal dependencies; each provider module isolates its SDK blast radius. Releases are published with checksums and SLSA provenance via GoReleaser.

Report vulnerabilities privately via [GitHub Security Advisories](https://github.com/xavidop/mamori/security/advisories/new) - never in a public issue.

## Out of scope

`mamori` is not a secrets store and provides no encryption at rest or server component. Protecting the backends it reads from (IAM policies, Vault ACLs, KMS keys) is your infrastructure's responsibility.

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
