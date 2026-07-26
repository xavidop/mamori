# mamori CLI (`cmd/mamori`)

`github.com/xavidop/mamori/cmd/mamori` is a standalone CLI built around the same source-tag conventions and `Report` shape the mamori library itself uses. It has two halves that never mix:

- **Static** (`explain`, `schema`, `policy`) - read Go source via `golang.org/x/tools/go/packages` and extract `source:` refs. No network calls, no secret manager contacted, nothing ever resolved: these commands answer "what does this config struct declare," not "what value does it hold."
- **Live** (`doctor`, `status`) - thin HTTP clients of a *running* process's admin endpoint (`mamori.WithAdminHTTP` / `mamori.Handler`). They read no source and render only whatever `mamori.Report` the target process's endpoint returns right now.

## Install

```bash
brew install xavidop/tap/mamori
# or
go install github.com/xavidop/mamori/cmd/mamori@latest
```

## Commands

```bash
mamori explain ./... [--type=Name] [--json]     # struct field table: type, chain, default, sensitive
mamori schema   ./... [--type=Name]              # JSON Schema (draft 2020-12) from field types + validate: tags
mamori policy   ./... --format=aws-iam|gcp|external-secret   # least-privilege access artifact

mamori doctor --endpoint <ep> [--json] [--compare ./...]     # one-shot: is the live process healthy?
mamori status  --endpoint <ep> [--watch] [--interval <dur>]  # render (and optionally keep watching) the same report
```

## Exit codes (doctor / status)

| Code | Meaning |
| --- | --- |
| `0` | Healthy |
| `1` | Unhealthy - reachable, but at least one field is stale or terminally errored |
| `2` | Reachable, but not a usable mamori admin API (404, or a 200 body that isn't a `mamori.Report`) |
| `3` | Unreachable - never got an HTTP response |
| `4` | Auth failed - reachable admin API, `401` |

## Docs

Full command reference, endpoint/auth flag details, and the CI-versus-incident split between `mamori.Doctor[T]` (library, pre-deploy) and `mamori doctor` (live, this CLI): [mamorigo.dev/docs/cli](https://mamorigo.dev/docs/cli).
