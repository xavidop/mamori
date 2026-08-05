# Provider lifecycle and TOML flatten

**Goal:** give every provider that holds a backend connection a way to release it, and
let `flatten` decode TOML alongside JSON, YAML and env.

**Status:** design approved, pending spec review.

Two independent changes, shipped together because both are small and both touch the
same documentation surfaces. Neither depends on the other; they can land as separate
PRs.

---

# Part 1: Provider resource lifecycle

## The gap

The `Provider` SPI (`provider.go:7`) is `Scheme` plus `Resolve`, extended by the
optional `WatchableProvider` and `BatchProvider`. Nothing in it releases a resource,
and nothing in core calls anything that would.

`Watcher.Close()` (`reconciler.go:143`) cancels the watch context, shuts down the admin
server and waits on its WaitGroup. It never touches a provider. Grepping
`reconciler.go`, `reconcile.go`, `registry.go` and `resolve.go` for `io.Closer` or a
provider `Close` call returns nothing.

The result is that a provider's backend client outlives every use of it. The sharpest
case is `providers/postgres`: `backendFor` builds a `pgxpool.Pool` lazily on first
resolve (`providers/postgres/postgres.go:250`) and `*Provider` has no `Close` at all, so
a `Watch` then `Close` cycle leaves a pool with live TCP connections that nothing can
reach. `mongodb`, `etcd`, `sqlite` and `k8s` are the same shape.

Two providers are worse than merely missing a `Close`: they already have a working one
on an unexported adapter that no caller can reach.
`providers/redis/redis.go:92` has `func (a universalAdapter) Close() error` and
`providers/launchdarkly/launchdarkly.go:119` has `func (r realClient) Close() error`.
The capability exists and is unreachable.

Nine providers do expose `Close() error` on `*Provider` already (`gcp`, `gcs`, `mysql`,
`firestore`, `configcat`, `firebase-rc`, `growthbook`, `split`, `unleash`), but
inconsistently and without a stated contract. No documentation page anywhere states who
owns a provider's lifetime: `WithProvider`'s doc comment (`reconcile.go:118`) says only
that it "registers a provider for this call only", and searching the docs tree and every
provider README for ownership guidance returns nothing relevant.

`providertest` does not exercise `Close`. Its goroutine-leak case runs `goleak` over
`Watch`, which catches a leaked goroutine but not a leaked connection pool.

This is the project's one lifecycle blind spot. Everything mamori itself creates is
released carefully, down to the comment in `server/server.go:141` explaining why
`closed` cannot be a bare `sync.Once`. Resources the *provider* creates are nobody's
job.

## Who closes: the caller, always

Core does not close providers. `Watcher.Close()` is unchanged.

The decisive constraint is that `Register` (`registry.go:18`) stores providers in a
process-global map, normally from a package `init`. Those instances are shared by every
`Watcher` in the process. A `Watcher.Close()` that closed them would hand the next
`Watch` call a dead client.

`WithProvider` (`reconcile.go:120`) instances are per-call, so closing those would be
defensible, but it would silently break the reasonable pattern of building one provider
and passing it to two watchers. Making core close *some* providers and not others also
means the rule a user has to remember is "it depends on how you registered it", which is
worse than one rule that always holds.

So: **the caller owns every provider instance it constructs, and closes it.**

```go
p := postgres.New(postgres.WithDSN(dsn))
defer p.Close()      // caller owns p

w, err := mamori.Watch[Config](ctx, mamori.WithProvider(p))
defer w.Close()      // closes only what mamori created
```

This adds no core API. Providers standardise on the stdlib `io.Closer` signature rather
than a mamori-specific interface, because `Close() error` is exactly `io.Closer` and a
named alias would earn nothing.

## The contract

A provider `Close` must be:

- **Idempotent.** Calling it twice is not an error and must not panic.
- **Safe with no prior use.** `New()` then `Close()` with no `Resolve` in between must
  not dial, block or panic. This is a real case, not a hypothetical: postgres builds its
  pool on first resolve, so a provider constructed for a config path that turned out to
  be unused has no pool to close.
- **Concurrency-safe.** Callable while a `Resolve` is in flight, and while another
  goroutine is calling `Close`.
- **Terminal for `Resolve`.** After `Close`, `Resolve` returns an error rather than
  panicking on a nil client. The kind is `mamori.ErrUnavailable`: the backend genuinely
  cannot be reached through this provider any more.

`Close` is deliberately *not* required to stop a `Watch`. Watches already end on context
cancellation, which is the existing, conformance-tested mechanism
(`providertest.go:217`, `testWatchCloses`). Making `Close` a second, competing shutdown
path would create an ordering question with no good answer.

## Scope

All 45 provider modules audited (`providers/` holds 46 directories; `httpcore` is a
shared helper, not a provider). Three tiers.

**Tier 1, holds a real closable resource and has no `Close` today.** Add one.

| Module | Resource |
|---|---|
| `postgres` | `pgxpool.Pool`, built lazily |
| `mongodb` | `mongo.Client` (`Disconnect`) |
| `etcd` | `clientv3.Client` |
| `redis` | closable client behind `universalAdapter` |
| `sqlite` | `sql.DB` |
| `launchdarkly` | LD client behind `realClient` |
| `k8s` | `kubernetes.Interface` transport idle connections |

**Tier 2, owns only an `*http.Client`.** Add `Close() error` calling
`CloseIdleConnections()`: `bitwarden`, `cloudflare-kv`, `doppler`, `heroku`, `https`,
`infisical`, `hcp-vault-secrets`, `nacos`, `onepassword`, `posthog`, `scaleway-sm`,
`supabase`, `vercel-gc`, `mamori`, `firebase-rtdb`.

`CloseIdleConnections` is close to a no-op, and the reason to add it anyway is
ergonomic rather than resource-driven: a caller should be able to write `defer p.Close()`
against any provider that owns a connection, without type-asserting `io.Closer` on each
one to discover whether this particular module happens to need it. It also matters for
the providers that let a caller inject a custom `*http.Client` with its own `Transport`.

**Tier 3, genuinely stateless.** No change: the core built-ins (`env`, `file`, `exec`,
`dotenv`) and `sops`, `viper`, `aws`, `azure`, `azblob`, `s3`, `cosmos`, `dynamodb`,
`vault`, `consul`, `flagsmith`, `flipt`, `goff`, `openfeature`. These hold no handle that
can be released. (`aws-appconfig` and `azure-appconfig` are schemes within the `aws` and
`azure` modules, not separate modules, and are covered by those entries.) A no-op `Close` here would assert a lifetime that
does not exist and invite cargo-culted `defer` lines.

**Already correct.** `gcp`, `gcs`, `mysql`, `firestore`, `configcat`, `firebase-rc`,
`growthbook`, `split`, `unleash`: audit each against the contract above (particularly
idempotency and close-without-use) and fix any that fall short. No new `Close`.

## Enforcement

A new `providertest` case, run only when the provider under test satisfies `io.Closer`,
so tier 3 modules are unaffected:

- `Close` on a freshly constructed provider that has never resolved returns without
  error and without dialing.
- `Close` called twice returns without error.
- `Resolve` after `Close` returns an error satisfying `errors.Is(err,
  mamori.ErrUnavailable)`.
- `Close` concurrent with an in-flight `Resolve` does not race (exercised under `-race`).

The suite already runs `goleak`; the closer case runs inside that same envelope, so a
`Close` that leaks a goroutine fails too.

---

# Part 2: TOML flatten

## The gap

`decodeFlatten` (`decode.go:265`) supports `json`, `yaml` and `env`, and returns
`unknown flatten %q` for anything else. TOML is a mainstream Go configuration format and
the one common file format a `file://` or `s3://` payload can arrive in that mamori
cannot decode.

## Change

Add `github.com/pelletier/go-toml/v2` as a direct core dependency and one `case` to
`decodeFlatten`:

```go
case "toml":
    if err := toml.Unmarshal(raw, &m); err != nil {
        return fmt.Errorf("mamori: field %s: toml flatten: %w", spec.Path, err)
    }
```

It decodes into the same `map[string]any` the other formats produce and hands off to the
existing mapstructure decoder, so `flattenHook` (secret and duration coercion,
`decode.go:301`) and any user `WithDecodeHook` apply unchanged, in the same order. TOML's
native typed scalars (`int64`, `float64`, `bool`, `time.Time` datetimes) reach
mapstructure with `WeaklyTypedInput` already set, the same path YAML's typed scalars take
today.

A malformed payload is a loud error, matching the existing json and yaml arms rather
than decoding to an empty struct.

## Dependency

`pelletier/go-toml/v2` is pure Go with no transitive dependencies, actively maintained,
and the TOML parser Viper uses. Core currently has six direct dependencies; this is the
seventh, at the same tier as the `gopkg.in/yaml.v3` already present for `flatten:"yaml"`.

Adding it to core rather than an optional module is what keeps `flatten:"toml"` a
one-word tag. A pluggable codec registry was considered and rejected as unnecessary
surface for a closed set of formats that changes rarely.

## Sites enumerating the format set

Four in code, all needing the new value:

- `decode.go:23`, the `Flatten` field comment.
- `decode.go:143`, the missing-tag error text
  (`add flatten:"json|yaml|env"`).
- `decode.go:262`, `decodeFlatten`'s doc comment.
- `reconcile.go:128`, `WithDecodeHook`'s doc comment.

---

# Testing

**TOML.** Table tests beside the existing flatten tests: a nested TOML table into a
nested struct; `secret.String` and `time.Duration` fields coerced through `flattenHook`;
a user `WithDecodeHook` still applying on the TOML path; malformed TOML producing a loud
error rather than a zero struct; TOML's native datetime and integer types surviving
mapstructure.

**Provider lifecycle.** The `providertest` closer case above, plus per-module tests for
each newly added `Close`: constructed-then-closed with no resolve (the postgres lazy-pool
case, asserted to perform no dial), double close, and post-close `Resolve` returning
`ErrUnavailable`. Tier 2 modules get a test that `Close` returns nil and leaves the
provider safe to close again.

# Documentation

Both features ship with docs in the same change.

**TOML:** the tag table in `site/src/pages/docs/concepts/index.md:53`
(`flatten:"json|yaml|env"`); a worked example in
`site/src/pages/docs/providers/file.md`, which already demonstrates
`flatten:"yaml"`; and `site/src/pages/docs/concepts/decoding.md`.

**Lifecycle:** a new ownership section in `site/src/pages/docs/writing-a-provider/`
stating the contract and that the caller closes; a line in the conformance checklist in
`site/src/pages/docs/writing-a-provider/conformance.md`; a `Close` entry in each affected
provider README and its `site/src/pages/docs/providers/<name>.md` page; and a
`defer p.Close()` in the `WithProvider` examples that construct a stateful provider.

**Both:** check `skills/mamori/` for the tag table and any provider-construction example,
and update `README.md`. The dependency sentence at `README.md:235` names three core
dependencies and already omits `yaml.v3`; correct it to match reality while adding TOML.

# Out of scope

- Any change to `secret.String`. The `MarshalJSON` redaction asymmetry stays as it is.
- Any change to core's public API, including `Watcher.Close()` and the `Provider` SPI.
- `Close` on stateless providers.
- Closing a provider as a way to stop a watch; context cancellation remains the only
  shutdown path.
