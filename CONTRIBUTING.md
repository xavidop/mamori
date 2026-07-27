# Contributing to mamori

Thanks for your interest! Contributions - especially new providers - are very welcome.

## Ground rules

- Be kind. See the [Code of Conduct](CODE_OF_CONDUCT.md).
- **Never commit real secrets** - not in code, tests, fixtures, or issue reports.
- Discuss significant changes in an issue first so we agree on scope before you build.

## Repository layout

This is a **multi-module monorepo** unified by a `go.work` file:

- Root module `github.com/xavidop/mamori` - core: `Load`/`Watch`, the reconciler, secret types, built-in providers (`env`/`file`/`exec`), `middleware/`, and the `providertest/` conformance kit. Its dependencies are deliberately minimal (validator, mapstructure, fsnotify).
- `providers/<name>` - each provider is its **own module** so cloud SDKs never reach core consumers.
- `x/otel` - the OpenTelemetry bridge.
- `cmd/mamori` - the `mamori` CLI. It also ships the `mamori vet` `go vet` analyzer (`internal/vetcheck`) and the stdlib-only `source:` tag parsing they share (`internal/sourcetag`).
- `site/` - the Astro documentation site.

## Development

```bash
# Core (and its subpackages)
go test ./...
go vet ./...

# A provider module - run with the workspace disabled so you only touch its go.mod
cd providers/aws
GOWORK=off go mod tidy && GOWORK=off go test ./...

# Everything at once
make test        # test all modules
make lint        # golangci-lint + go vet across modules
```

Requires **Go 1.26+**.

## Writing a provider

Read the [Write a provider guide](https://mamorigo.dev/docs/writing-a-provider) - it is the complete contract. In short:

1. Create `providers/<name>/` as its own module with `replace github.com/xavidop/mamori => ../..`.
2. Implement `mamori.Provider` (`Scheme()` + `Resolve`). Add `WatchableProvider` **only** if the backend has native change notification; otherwise mamori polls. Add `BatchProvider` if the backend can resolve many refs in one call.
3. Classify errors beyond not-found: wrap SDK errors with mamori's sentinels (`ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`) using `%w`, not `%v`, so `errors.Is` keeps working. This is two separate requirements: passing the `ErrorClassification` conformance case (step 5) only proves your mapping is not destroyed in transit, not that it exists; you must also add a table test that maps real SDK error values to kinds. See [Classifying errors](https://mamorigo.dev/docs/writing-a-provider#classifying-errors).
4. Return an error satisfying `errors.Is(err, mamori.ErrNotFound)` for missing values. Set `Value.Version` (native revision or `mamori.VersionHash`). Set `Value.Sensitive = true` for secret managers. Never log payloads.
5. Make the provider testable by injecting a client interface, and **pass the conformance kit**:

   ```go
   func TestConformance(t *testing.T) {
       providertest.Run(t, providertest.Config{
           New:    func() mamori.Provider { return newWithClient(fake) },
           Ref:    func(k string) string { return "myscheme://" + k },
           Seed:   func(ctx context.Context, key, val string) error { ... },
           Mutate: func(ctx context.Context, key, val string) error { ... },
           Fail:   func(ctx context.Context, key string, err error) error { ... },
           Clear:  func(ctx context.Context, key string) error { ... },
       })
   }
   ```

   `Fail` and `Clear` are **required** so the `ErrorClassification` case runs; a
   provider that supplies neither now fails `providertest.Run` outright. The
   only exemption is a backend with no per-key error surface at all (existence
   is a bool or a sentinel value, nothing to inject) - set
   `NoResolveErrors: true` instead, with a comment naming why. Live-backend
   `//go:build integration` tests, which cannot inject errors against a real
   backend, also set `NoResolveErrors: true` (the unit-test run against the
   fake already covers classification).

   If your `#key` fragment selects into a JSON payload via `mamori.SelectKey`,
   also supply `providertest.Config.PointerRef` so the `JSONPointerSelection`
   case exercises RFC 6901 JSON Pointer selection (`#/credentials/password`,
   `#/replicas/5/host`) against your provider. Leave `PointerRef` `nil`, with a
   comment naming why, when the fragment is a backend-native key, a facet
   selector on an evaluated result, or unused - the case skips rather than
   failing.

   The kit also runs `DecodeOption` unconditionally (no opt-in, no config
   field to wire): it asserts your `Resolve` passes an unrecognized query
   option like `?decode=base64` through untouched, since decoding a resolved
   value is entirely core's job (`applyDecode`), not the provider's. This
   normally requires no extra work - it only fails if your provider inspects,
   strips, or rejects query options it does not itself recognize.

6. Add a `README.md` documenting schemes, ref grammar, auth, and what is verified vs needs a live backend. A provider that passes `providertest` gets listed in the root README with a badge.
7. If you classified errors beyond not-found (step 3), also: add an `## Error classification` section to the module's `README.md`, mirror it onto the module's docs-site page, and flip that module's row in both coverage tables (the root `README.md` and `site/src/pages/docs/providers/index.md`).

## Commit & PR

- Keep PRs focused. Reference the issue they resolve.
- Follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `feat(aws):` …) - the changelog is generated from them.
- Fill out the PR template checklist.
- CI must be green (tests, vet, lint) before review.

## Releases

Core releases are automated from [Conventional Commits](https://www.conventionalcommits.org/), so your commit messages matter:

- `fix:` -> patch release, `feat:` -> minor, `feat!:` / `BREAKING CHANGE:` -> major.
- `docs:`, `chore:`, `test:`, `refactor:` do not trigger a release on their own.

When such commits land on `main`, [`semantic-release`](https://semantic-release.gitbook.io/) determines the next version, updates `CHANGELOG.md`, and creates + pushes the `vX.Y.Z` tag. [GoReleaser](https://goreleaser.com/) then builds the `mamori` CLI binary and publishes the GitHub Release (artifacts, checksums, SBOM), and SLSA provenance is generated. See [`.releaserc.json`](.releaserc.json), [`.goreleaser.yaml`](.goreleaser.yaml), and [`.github/workflows/release.yml`](.github/workflows/release.yml).

Provider submodules keep their own tags (`providers/aws/v0.1.0`, `x/otel/v0.1.0`, ...) and are released separately, so a breaking change in one SDK never forces a core release.
