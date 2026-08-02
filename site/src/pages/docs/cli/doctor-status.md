---
layout: ../../../layouts/DocsLayout.astro
title: mamori doctor and status
---

# mamori doctor and status

Both are thin clients of a running process's admin endpoint (`mamori.WithAdminHTTP` / `mamori.Handler`, see [Observability](/docs/observability/)). They GET `/`, render the `mamori.Report` that comes back, and resolve nothing themselves. `doctor` is a one-shot health check with drift detection; `status` is the same table, optionally on a loop.

For the library-side preflight you run in CI before deploying, see [Doctor: pre-deploy check](/docs/observability/doctor/). That runs your exact wiring inside your build; `mamori doctor` runs from anywhere against a process that is already running.

## doctor

```bash
mamori doctor --endpoint <ep> [--insecure] [--json] [--compare "<patterns>"]
```

- `--json` emits the admin endpoint's raw response body unchanged, byte for byte, instead of a table.
- `--compare` takes space-separated Go package patterns. It statically extracts `source:` refs (the same walk [`explain`](/docs/cli/explain/) uses) and flags any field present in source but missing from the live report, or present live but not in source. It needs a buildable source tree and does not change the exit code.

```bash
$ mamori doctor --endpoint https://svc.internal:9090 --compare ./...
PATH            SCHEME  REF                     VERSION   STALE  LAST_KIND  LAST_ERROR  SENSITIVE  DERIVED
Redis.Addr      env     env://REDIS_ADDR        3         false  -          -           false      false
Redis.Password  aws-sm  aws-sm://prod/redis-pw  3         false  -          -           true       false
DSN                                             a3f9c1e2  false  -          -           true       true

HEALTHY: 3 field(s), snapshot 3 (live 3), generated 2026-07-26T10:00:00Z

compare: source vs. live field paths
  no drift: source and live field sets match
```

A row with `DERIVED` `true` is a [`WithDerive`](/docs/usage/derived-fields/) write path, not a field mamori resolved. `SCHEME` and `REF` are empty because there is no ref behind it; `VERSION` is a content hash of the value the hook produced. `--compare` ignores these rows, so a derive never reports as drift.

## status

Renders the same report table as `doctor`, without `--compare` or `--json`.

```bash
mamori status --endpoint <ep> [--insecure] [--watch] [--interval <dur>]
```

- `--watch` renders immediately, then again on every tick of `--interval` (default `2s`) until interrupted with Ctrl-C.
- `--interval <dur>` sets the poll interval when `--watch` is set (e.g. `5s`).

```bash
$ mamori status --endpoint unix:///run/app-admin.sock --watch --interval 5s
PATH        SCHEME  REF               VERSION   STALE  LAST_KIND  LAST_ERROR  SENSITIVE  DERIVED
Redis.Addr  env     env://REDIS_ADDR  3         false  -          -           false      false
DSN                                   a3f9c1e2  false  -          -           true       true

HEALTHY: 2 field(s), snapshot 3 (live 3), generated 2026-07-26T10:00:00Z
# ...re-renders every 5s until Ctrl-C
```

## Exit codes

Both live commands share one exit-code table, so a script can tell "my config is broken" (`1`) apart from "I couldn't even see my config" (`2`/`3`/`4`).

| Code | Meaning |
| --- | --- |
| `0` | Healthy: every field is fresh with no terminal error |
| `1` | Unhealthy: reachable, but at least one field is stale or terminally errored |
| `2` | Reachable, but not a usable mamori admin API: a 404, or a 200 body that does not decode as a `mamori.Report` |
| `3` | Unreachable: never got an HTTP response (connection refused, no such socket, TLS failure, or a malformed `--endpoint`) |
| `4` | Auth failed: a reachable mamori admin API returned `401` |

Branch on this directly: `1` means fix the config; `2`/`3` mean fix connectivity (or wire up `mamori.WithAdminHTTP`); `4` means fix the credential. `--watch` never returns these mid-loop: each poll's outcome is only printed, and the command returns `0` once you interrupt it.

## Endpoint forms

`--endpoint` accepts three forms, matching what the admin endpoint and the [config server](/docs/server/) serve:

- `https://host:port`
- `unix:///path/to.sock`
- `http://host:port`, only with `--insecure` passed explicitly; a plain `http://` endpoint is refused otherwise.

## Auth flags

Credentials reuse the same `Authenticator` schemes the admin endpoint supports. Bearer and basic are mutually exclusive.

| Flag | Purpose |
| --- | --- |
| `--bearer` / `--bearer-file` | Bearer token, as a value or a file path (`-` for stdin) |
| `--basic` / `--basic-file` | `user:pass`, as a value or a file path (`-` for stdin) |
| `--client-cert` / `--client-key` | Client certificate and key (PEM) for mTLS |

**Prefer the file/stdin forms.** `--bearer`/`--basic` put the credential in `os.Args`, visible to anything that can read this process's argv (e.g. `ps`). The `-file` forms (including `-` for stdin) keep the token out of both shell history and argv.

## Custom provider schemes with --compare

`--compare` is the only part of `doctor` that reads source, so it is the only part affected by the scheme set. If you [wrote your own provider](/docs/writing-a-provider/), name its scheme so drift detection classifies its fields as secrets:

```bash
mamori doctor --endpoint https://svc:9090 --compare ./... --secret-schemes=mysecrets
```

The flag is validated before any network call, so a typo fails immediately rather than after the round trip. See [`mamori vet`](/docs/cli/vet/#covering-a-custom-provider) for the built-in set.

## See also

[CLI overview](/docs/cli/). [Observability](/docs/observability/) covers the `Report` shape these render; [Config server](/docs/server/) shares the same endpoint forms and auth schemes.
