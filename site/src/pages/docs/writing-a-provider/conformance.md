---
layout: ../../../layouts/DocsLayout.astro
title: Conformance
---

# Conformance

`github.com/xavidop/mamori/providertest` runs one function that exercises resolution, not-found typing, error classification, `Version` monotonicity, concurrency, context cancellation, native watch, goroutine hygiene (goleak), and a no-payload-logging assertion. Every provider must pass it.

## Run the conformance case

Inject a client interface so the kit and your unit tests run against an in-memory fake, with live-backend tests behind a `//go:build integration` tag:

```go
func TestConformance(t *testing.T) {
	backend := newInMemoryFake()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return myprovider.New(myprovider.WithClient(backend)) },
		Ref:    func(key string) string { return "myscheme://" + key },
		Seed:   func(ctx context.Context, key, val string) error { return backend.set(key, val) },
		Mutate: func(ctx context.Context, key, val string) error { return backend.set(key, val) },
		Fail:   func(ctx context.Context, key string, err error) error { return backend.fail(key, err) },
		Clear:  func(ctx context.Context, key string) error { return backend.clear(key) },
	})
}
```

`Fail` and `Clear` are **required**: `Fail` makes the fake return a given error for a key, `Clear` cancels it. They power the `ErrorClassification` case, which injects a mamori sentinel through your fake and checks it returns from `Resolve` with its `errors.Is` chain intact (catching the `%v`-instead-of-`%w` bug). A provider supplying neither `Fail` nor `NoResolveErrors: true` fails `providertest.Run` outright.

Set `NoResolveErrors: true` (with a comment naming why) only if your backend has no per-key error surface. Live integration tests also set it, since the unit-test run already covers classification against the fake.

Classification is guarded twice: the conformance case cannot prove your SDK mapping exists (no fake produces real SDK errors), so add a table test over real SDK errors too. `KindUnknown` is always a legal outcome.

## Build and test the module

Build and test each module independently with the workspace disabled, exactly as CI does:

```bash
cd providers/<name>
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```

Or from the repo root, `make test` / `make lint` run every module.

## Acceptance checklist

- [ ] `Scheme()` returns your scheme; `Resolve` returns `mamori.ErrNotFound` (via `errors.Is`) for missing values.
- [ ] Other failures map to a sentinel (`ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`) with `%w`. Cover it two ways: `Fail`/`Clear` in `providertest.Config` (required unless `NoResolveErrors: true` with a reason), AND a table test mapping real SDK errors to kinds.
- [ ] `Value.Version` is set and changes when the value changes; secret-bearing values set `Sensitive: true`.
- [ ] `#key` uses `mamori.SelectKey`; the payload is never logged.
- [ ] `Watch` is implemented only for native-push backends, closes on `ctx` cancel, and leaks no goroutines.
- [ ] A client interface is injected so `providertest.Run` passes against a fake; live tests are behind `//go:build integration`.
- [ ] `go build`, `go vet`, and `go test` are clean with `GOWORK=off`; the README documents scheme, ref grammar, and auth.
- [ ] The module's `README.md` has an `## Error classification` section, mirrored onto its docs-site page.
- [ ] The module's row is flipped in both coverage tables: root `README.md` and `site/src/pages/docs/providers/index.md`. Error classification lives in three places that drift if edited alone (module `README.md`, its docs-site page, and the two coverage tables); update them together.

See also: [Resolve and errors](/docs/writing-a-provider/resolve/), [Testing](/docs/testing/).
