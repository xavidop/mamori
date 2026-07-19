# mamori Provider SPI - author brief

This is the contract every provider module implements. The core module is at the
repo root (`github.com/xavidop/mamori`); read these files for the exact, current
signatures before writing code:

- `provider.go` - `Provider`, `WatchableProvider`, `BatchProvider`, `Update`
- `value.go` - `Value`
- `ref.go` - `Ref`, `ParseRef`, `Ref.Opt(name)`
- `errors.go` - `ErrNotFound`, `ProviderError`
- `helpers.go` - `VersionHash([]byte) string`, `SelectKey(data []byte, key string) ([]byte, error)`
- `registry.go` - `Register(Provider)`
- `builtin_file.go` - a reference implementation of a `WatchableProvider`
- `providertest/providertest.go` - the conformance kit your module must pass

## The interfaces (as of freeze)

```go
type Provider interface {
    Scheme() string
    Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)
}
type WatchableProvider interface {              // optional
    Provider
    Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error)
}
type BatchProvider interface {                  // optional
    Provider
    ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) // keyed by ref.Raw
}
type Update struct { Value mamori.Value; Err error } // channel close = watch ended

type Value struct {
    Bytes     []byte
    Version   string        // native revision, or mamori.VersionHash(bytes)
    Sensitive bool          // true for secret managers
    NotAfter  time.Time     // lease expiry, if known (drives pre-expiry refresh)
    Metadata  map[string]string
}

type Ref struct {
    Scheme string            // your scheme
    Path   string            // provider path
    Key    string            // #key (use mamori.SelectKey for JSON payloads)
    Opts   url.Values        // ?opt=v  (Ref.Opt("name"))
    Raw    string
}
```

## Rules every provider MUST follow

1. `Resolve` returns an error satisfying `errors.Is(err, mamori.ErrNotFound)` when
   the value does not exist. Never return a nil error with empty bytes for missing.
2. When `ref.Key != ""` and the payload is a JSON object, call
   `mamori.SelectKey(payload, ref.Key)` to select the field, identically to every
   other provider.
3. Set `Value.Version` to the backend's native revision when available
   (VersionId, ResourceVersion, ModifyIndex, ModRevision, secret version id);
   otherwise `mamori.VersionHash(bytes)`. Version MUST change when the value changes.
4. Secret-manager providers set `Value.Sensitive = true`. Config-style providers
   (Parameter Store String, ConfigMap, Consul KV, databases) set it false unless
   the value is a secure/encrypted type.
5. NEVER log the payload. The conformance kit asserts this.
6. `Watch` (if implemented) must close its channel when `ctx` is cancelled and
   must not leak goroutines (the kit uses goleak). Implement `Watch` ONLY if the
   backend has native change notification; otherwise mamori polls.
7. Honor `ctx` in every network call.
8. If your provider builds queries from ref parts (databases), bind the key as a
   query parameter and validate any table/column identifiers against a strict
   allowlist. Never string-concatenate ref input into a query.

## Module layout (each provider is its OWN module)

Create under `providers/<name>/`:

```
providers/<name>/
  go.mod          module github.com/xavidop/mamori/providers/<name>
  <name>.go       implementation
  <name>_test.go  unit tests + providertest self-test against an in-memory fake
  README.md       schemes, ref examples, auth, verified-vs-needs-live, badge
```

`go.mod` must contain, exactly:

```
module github.com/xavidop/mamori/providers/<name>

go 1.26

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..

require ( /* your SDK, added by go get */ )
```

## CRITICAL build/test instructions

- The repo root has a `go.work`. Run EVERY go command in your module with the
  workspace disabled, so you touch only your own module files:

  ```
  cd providers/<name>
  GOWORK=off go mod tidy
  GOWORK=off go build ./...
  GOWORK=off go test ./...
  GOWORK=off go vet ./...
  ```

- `replace github.com/xavidop/mamori => ../..` resolves the core module locally.
- Use a version like `v0.1.0` in the `require`; the replace makes the exact
  version irrelevant for local builds.

## Testability pattern (so conformance runs without a live backend)

Define a minimal client interface for the SDK calls you use. The real SDK client
satisfies it. `New(opts ...Option)` builds the real client lazily (ambient
credential chain). Provide an option to inject an in-memory fake implementing the
interface. Then your `<name>_test.go` runs:

```go
providertest.Run(t, providertest.Config{
    New:    func() mamori.Provider { return newWithClient(fake) },
    Ref:    func(k string) string { return "<scheme>://" + k },
    Seed:   func(ctx context.Context, key, val string) error { fake.set(key, val); return nil },
    Mutate: func(ctx context.Context, key, val string) error { fake.set(key, val); return nil },
})
```

If the backend has no native watch, set `SkipWatch: true` (or just don't
implement Watch - the kit skips watch tests automatically for non-watchable
providers). If it does, make the fake support the watch so the checks run for real.

## Registration

Provide `func New(opts ...Option) *Provider` and, if the provider can operate from
ambient credentials/env, an `init()` that registers a lazily-initialized instance:

```go
func init() { mamori.Register(New()) }
```

Document that users who need explicit config call
`mamori.WithProvider(<name>.New(<name>.WithRegion("...")))`.

## Acceptance checklist

- [ ] `GOWORK=off go build ./...` passes
- [ ] `GOWORK=off go vet ./...` passes
- [ ] `GOWORK=off go test ./...` passes, including the `providertest.Run` self-test
- [ ] README documents schemes, ref examples, auth, and what is verified vs needs
      a live backend
