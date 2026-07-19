# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately**. Do not open a public issue, discussion, or pull request for a security problem.

Use GitHub's private vulnerability reporting: **[Report a vulnerability](https://github.com/xavidop/mamori/security/advisories/new)**. This opens an advisory visible only to you and the maintainers.

Include as much as you can:

- A description of the issue and its impact.
- Steps to reproduce, or a proof of concept.
- The affected module(s) and version(s).
- Any suggested remediation.

Please do not include real secret values in your report.

## What to expect

- Acknowledgement within 3 business days.
- An initial assessment and severity rating within 7 days.
- Coordinated disclosure: we will work with you on a fix and a release, and credit you (if you wish) in the advisory and release notes.

## Supported versions

mamori is a multi-module monorepo: the core module and each provider module are versioned independently. Security fixes land on the latest release of the affected module; upgrade that module to its newest version.

| Module | Supported |
| --- | --- |
| core (`github.com/xavidop/mamori`) | latest release |
| `providers/*`, `x/otel`, `tools/reconcilevet` | latest release |

## Scope

In scope:

- The core library and the provider / tooling modules in this repository.
- Secret handling: unintended exposure of secret values (through logs, errors, or serialization), the redaction contract of `secret.String` / `secret.Bytes`, and the `reconcilevet` analyzer.
- The `exec:` provider's command handling and injection surface.

Out of scope:

- Vulnerabilities in third-party provider SDKs or backends. Report those upstream; we will update our dependency once a fix is available.
- Issues that require an already-compromised host, or credentials the reporter already controls.

## Security model

mamori is designed to keep secrets out of logs and errors by default:

- `secret.String` / `secret.Bytes` render as `[REDACTED]` in `String()`, `fmt`, `encoding/json`, and `log/slog`; only the explicit, greppable `Reveal()` exposes the value.
- The shipped `reconcilevet` analyzer flags secret-bearing refs assigned to plain `string` / `[]byte` fields.
- Providers must never log ref payloads; the `providertest` conformance kit asserts this.
- The `exec:` provider is disabled by default, and refs are never interpolated from other resolved values (no injection chains).
- `Zero()` is best-effort; Go's garbage collector may retain copies, and this is documented honestly rather than promised.

See the [security documentation](https://mamorigo.dev/docs/security) for the full model.
