---
layout: ../../../layouts/DocsLayout.astro
title: Conformance
---

# Conformance

`github.com/xavidop/mamori/providertest` runs one function that exercises resolution, not-found typing, nested JSON Pointer selection (opt-in), `?decode=` option passthrough, error classification, `Version` monotonicity, concurrency, context cancellation, native watch, goroutine hygiene (goleak), the `Close` contract (for a provider that implements `io.Closer`), and a no-payload-logging assertion. Every provider must pass it.

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

## `JSONPointerSelection`

This case proves your provider routes `#key` through `mamori.SelectKey` rather than hand-rolling a top-level-only lookup, so RFC 6901 JSON Pointer selection (`#/credentials/password`, `#/replicas/5/host`) behaves identically everywhere. It is **opt-in**, because a fragment does not mean the same thing in every provider:

```go
providertest.Config{
	// ...
	PointerRef: func(key, frag string) string {
		return "myscheme://" + key + frag
	},
}
```

Supply `PointerRef` only when your provider's fragment slot **is** a JSON selector into a structured payload. Leave it `nil`, with a comment naming why, when the fragment is something else entirely - a backend-native key (a Kubernetes Secret's `data` map entry, a Doppler secret name), a facet selector on an evaluated result (`flipt`'s `#attachment`, `unleash`'s `#variant`/`#payload`), or nothing at all (`providers/mamori` never reads `ref.Key`). None of those is a defect; the case simply skips.

`PointerRef` is a ref *builder*, not a boolean, because `Config.Ref` is not fragment-free by convention - several providers bake a fixed fragment into it (`vault://secret/<key>#value`, and the same shape in `mongodb`, `firestore`, and `k8s`), so appending a second fragment to `Ref`'s output would produce a doubled, unparseable fragment. The builder lets each provider say exactly where its selector goes.

## `DecodeOption`

Decoding a resolved value (`?decode=base64`, `?decode=base64,gzip`, ...) is entirely core's job: `applyDecode` runs on every `Value` at every point it enters the engine, and no provider ever inspects or acts on `?decode=` itself. Unlike `JSONPointerSelection`, this case is **not opt-in** - it runs unconditionally for every provider.

What it catches is narrower and easy to miss otherwise: a provider that strips, rewrites, or errors on a query option it does not recognize. That bug would pass every other case in this kit and still silently break `?decode=` (and any future core-owned option) for that provider's users. The rule it enforces:

**A provider must pass an unrecognized query option through untouched.** `DecodeOption` seeds a value that is itself a base64 string, resolves it through a ref carrying `?decode=base64`, and asserts the provider hands back exactly the stored (still-encoded) bytes - proving the provider read `ref.Path`/`ref.Key` and ignored the option rather than rejecting it or trying to interpret it itself.

## `Close`

Give your provider a `Close` method and the kit runs the [Close contract](/docs/writing-a-provider/#the-contract) against it. A provider without one skips this case rather than failing it, so there is nothing to configure either way.

It closes a provider that has never resolved (which must not dial or panic), then resolves, closes twice, and checks that the next `Resolve` refuses locally and fast with `errors.Is(err, mamori.ErrUnavailable)` rather than quietly rebuilding the client it was just told to release. Both a slow refusal and the right error for the wrong reason fail here: a provider that redials a dead backend also reports unavailable, so the case bounds how long the refusal may take. It then races `Close` against resolves already in flight, which is why this case is worth running under `-race`.

One rule it cannot check for you is the last one in the contract: that `Close` leaves a client the caller injected open. The kit has no way to know which client you were handed, so that one needs a unit test in your own package.

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
- [ ] If your fragment is a JSON selector, supply `providertest.Config.PointerRef` so `JSONPointerSelection` runs; otherwise leave it `nil` with a comment naming why (backend-native key, facet selector, or unused).
- [ ] `Resolve` passes an unrecognized query option (e.g. `?decode=`) through untouched rather than stripping, rewriting, or erroring on it; `DecodeOption` proves this and runs unconditionally.
- [ ] `Watch` is implemented only for native-push backends, closes on `ctx` cancel, and leaks no goroutines.
- [ ] If the provider holds a releasable handle, it implements `io.Closer`; `Close` is idempotent, safe with no prior `Resolve`, concurrency-safe, terminal (`Resolve` afterwards reports `errors.Is(err, mamori.ErrUnavailable)`), and never closes a caller-injected client. `providertest.Run` exercises the first four automatically; the fifth needs a provider-specific unit test.
- [ ] A client interface is injected so `providertest.Run` passes against a fake; live tests are behind `//go:build integration`.
- [ ] `go build`, `go vet`, and `go test` are clean with `GOWORK=off`; the README documents scheme, ref grammar, and auth.
- [ ] The module's `README.md` has an `## Error classification` section, mirrored onto its docs-site page.
- [ ] The module's row is flipped in both coverage tables: root `README.md` and `site/src/pages/docs/providers/index.md`. Error classification lives in three places that drift if edited alone (module `README.md`, its docs-site page, and the two coverage tables); update them together.

See also: [Resolve and errors](/docs/writing-a-provider/resolve/), [Testing](/docs/testing/).
