# mamori — Typed, Watchable Config & Secrets for Go

**Status:** Approved design (build) · **Author:** Xavier (xavidop) · **Date:** 2026-07-18
**Module root:** `github.com/xavidop/mamori`

This document is the concrete, build-ready design derived from the v0.1 draft
(`reconcile` RFC). The project has been renamed **`mamori`** (守り — "protection /
safeguard"). The word *reconcile* survives as domain vocabulary: the engine
*reconciles* config, and change events are `Change[T]`.

---

## 1. Goals (unchanged from RFC)

1. **Typed loading** — one struct, tag-driven, multiple sources, generics API.
2. **Runtime reconciliation** — native watch where possible, poll where not;
   validated, diff-aware updates.
3. **Secret hygiene by default** — redaction in `String()`/`fmt`/`MarshalJSON`/logs;
   optional best-effort memory zeroization on rotation.
4. **Pluggable providers** via the `database/sql` registration pattern; core has
   **zero cloud SDK dependencies**.
5. **Conformance test kit** so third-party providers behave identically.
6. **Runs anywhere** — Lambda, systemd, Pod.

## 2. Key decisions locked for this build

| Decision | Choice |
|---|---|
| Name / module | `mamori` / `github.com/xavidop/mamori` |
| Repo layout | Multi-module monorepo + `go.work` |
| Build scope | Full breadth (core + all providers + middleware + kit + vet + docs + release) |
| Flagship providers | AWS (SM+PS), Vault, K8s, GCP, Azure — production-grade first |
| Docs aesthetic | Guardian / Japanese-modern (Astro, GH Pages via Actions) |
| Release | GoReleaser (binaries + changelog + SLSA provenance); libraries via git tags |
| Clock | Internal `mamori.Clock` interface (no `clockwork` dep in core) |
| Validation | `go-playground/validator/v10` (swappable via `WithValidator`) |
| Decode | `go-viper/mapstructure/v2` with secret-aware decode hooks |

### Deviations from the RFC (called out honestly)
- **Core dep set** is `stdlib + go-playground/validator/v10 +
  go-viper/mapstructure/v2 + fsnotify/fsnotify`. `fsnotify` enters core because
  §7.4 makes `file://` a built-in provider. Everything else in core is stdlib.
- **`exec:`** is NOT auto-registered. It lives in `provider/exec` and requires
  `mamori.WithExecProvider()` (RFC §9 security posture).
- **OTel** never enters core: `WithMeter`/`WithTracer` accept tiny internal
  interfaces; the real OTel bridge is a separate module `x/otel`.

---

## 3. Module layout

```
mamori/
  go.mod                         # core module
  go.work                        # local dev wiring for all modules
  reconcile.go                   # Load[T], Watch[T], Change[T], Watcher[T], options
  ref.go                         # Ref grammar parser
  registry.go                    # Register (database/sql pattern), lookup
  value.go                       # Value type
  provider.go                    # Provider / WatchableProvider / BatchProvider, Update
  reconciler.go                  # engine: debounce/coalesce/atomicity/backoff/dispatch
  poll.go                        # polling adapter (non-watchable -> pseudo-watch)
  decode.go                      # struct walk, tags, mapstructure wiring, defaults
  errors.go                      # ErrNotFound, ProviderError, ValidationError
  clock.go                       # Clock interface + system clock + test clock helper
  observ.go                      # internal Meter/Tracer interfaces (noop default)
  secret/                        # secret.String, secret.Bytes
  provider/env/                  # env:            (auto-registered, stdlib)
  provider/file/                 # file://         (auto-registered, fsnotify watch)
  provider/exec/                 # exec:           (opt-in, WithExecProvider)
  providertest/                  # conformance kit: Run(t, Config{New,Seed,Mutate})
  middleware/                    # Cache, Audit, Failover, RateLimit, Prefix
  providers/
    aws/         go.mod          # aws-sm://  aws-ps://       (aws-sdk-go-v2)
    gcp/         go.mod          # gcp-sm://                  (cloud.google.com/go)
    azure/       go.mod          # azure-kv://                (azsdk azsecrets)
    vault/       go.mod          # vault://   (native: lease NotAfter + KV v2 poll)
    k8s/         go.mod          # k8s-secret:// k8s-cm://    (client-go informer)
    consul/      go.mod          # consul://                  (consul/api)
    doppler/     go.mod          # doppler://                 (HTTP, no SDK)
    onepassword/ go.mod          # op://                      (Connect HTTP)
    sops/        go.mod          # sops://                    (age/KMS + fsnotify)
  x/otel/        go.mod          # OTel Meter/Tracer bridge
  tools/reconcilevet/ go.mod     # go vet analyzer (golang.org/x/tools)
  site/                          # Astro docs site
  .github/workflows/             # ci.yml, pages.yml, release.yml
  .goreleaser.yaml
```

Each `providers/*`, `x/otel`, and `tools/reconcilevet` is its **own Go module**
so cloud SDKs never leak into core consumers. `go.work` unifies them for local dev.

---

## 4. Ref grammar

```
source:"<scheme>://<path>[#<key>][?<opt>=<v>&...]"
source:"env:NAME"           # opaque-path scheme form
source:"exec:cmd arg ..."   # opaque-path scheme form
source:"file:///abs/path"
```

Parsed into:

```go
type Ref struct {
    Scheme string            // "aws-sm", "vault", "env", "file", ...
    Path   string            // "prod/db", "kv/data/api", "/etc/tls/tls.crt", "NAME"
    Key    string            // fragment after '#', "" if absent
    Opts   url.Values        // query params: renew, debounce, version, ...
    Raw    string            // original tag value, for errors
}
```

Rules:
- Hierarchical schemes use `scheme://path`. Opaque schemes (`env:`, `exec:`) take
  the remainder as `Path`.
- `#key` selects one field from a structured payload (JSON/YAML secret).
- `?opts` are provider-specific plus a small set of core-recognized ones:
  `debounce` (per-field override), `optional`, `version`.

Supplementary struct tags: `default`, `validate`, `flatten:"json|yaml|env"`,
`optional:"true"`.

---

## 5. Value, Provider SPI

```go
type Value struct {
    Bytes     []byte
    Version   string            // provider revision (cheap change detection)
    Sensitive bool
    NotAfter  time.Time         // zero = unknown; drives pre-expiry refresh
    Metadata  map[string]string
}

type Provider interface {
    Scheme() string
    Resolve(ctx context.Context, ref Ref) (Value, error)
}

type WatchableProvider interface {           // optional native push
    Provider
    Watch(ctx context.Context, ref Ref) (<-chan Update, error)
}

type BatchProvider interface {               // optional batch resolve
    Provider
    ResolveBatch(ctx context.Context, refs []Ref) (map[Ref]Value, error)
}

type Update struct { Value Value; Err error } // channel close = watch ended
```

Provider authors implement the smallest native interface. Core wraps
non-watchable providers in the **polling adapter**.

Registration (`database/sql` pattern), panics on duplicate scheme:

```go
func init() { mamori.Register(New()) }   // in each provider package
```

---

## 6. Secret types (`mamori/secret`)

```go
type String struct { b []byte }           // unexported
func (s String) Reveal() string
func (s String) String() string            // "[REDACTED]"
func (s String) MarshalJSON() ([]byte, error)  // "\"[REDACTED]\""
func (s String) LogValue() slog.Value      // redacted
func (s *String) Zero()                     // best-effort wipe
type Bytes struct { b []byte }              // same contract
```

mapstructure decode hooks populate `secret.String`/`secret.Bytes` from a `Value`
and set `Sensitive`. Assigning a sensitive ref to a plain `string`/`[]byte` field
is flagged by `reconcilevet`.

---

## 7. Public API

```go
cfg, err := mamori.Load[Config](ctx, opts...)                 // one-shot

w, err := mamori.Watch[Config](ctx,
    mamori.WithPollInterval(30*time.Second),
    mamori.WithJitter(0.2),
    mamori.WithDebounce(500*time.Millisecond),
    mamori.OnChange(func(ev mamori.Change[Config]) { ... }),   // ev.Old, ev.New, ev.Fields, ev.Changed("Path")
    mamori.OnError(func(err error) { ... }),
)
defer w.Close()
cfg := w.Get()   // lock-free atomic snapshot, always last *valid* config

// Options: WithProvider, WithValidator, WithDecodeHook, WithClock, WithMeter,
// WithTracer, WithExecProvider, WithQueueDepth, WithStale(maxAge), WithBackoff.
```

### Watch semantics (must be nailed; each has a test)
1. **Atomicity** — `OnChange` fires with a fully re-validated snapshot. A value
   that fails validation is **rejected**: `Get()` keeps the last good config,
   `OnError(ValidationError)` fires. No broken mid-flight state.
2. **Coalescing** — changes within the debounce window (default 500 ms;
   `?debounce=` per field) produce one `Change`.
3. **Ordering** — `OnChange` serialized on one goroutine; slow callback delays but
   never drops the next; bounded queue with drop-oldest guards pathological consumers
   (`WithQueueDepth`).
4. **First event** — `Watch` resolves initial config before returning (fail-fast);
   `OnChange` fires only on *subsequent* changes.
5. **Shutdown** — `Close()` cancels provider watches, drains the queue, returns.

---

## 8. Reconciler engine (heart of the system)

`Watch[T]`:
1. Initial `Load[T]` (fail-fast, never partial). Store in `atomic.Pointer[T]`.
2. For each ref: if provider is `WatchableProvider`, use native `Watch`; else wrap
   in the polling adapter (Clock ticker + jitter + `Version` compare). A non-zero
   `Value.NotAfter` schedules a refresh before expiry.
3. A single **reconciler goroutine** collects `Update`s → **debounce/coalesce** →
   build candidate snapshot (clone last-good, apply changed fields) → decode →
   **re-validate whole struct** → success: atomic swap + compute field diff + emit
   `Change` to dispatch queue; failure: reject atomically + `OnError`.
4. **Dispatch goroutine** delivers `OnChange` serially from a bounded queue
   (drop-oldest).
5. Resolve failures: retain last-good, per-ref exponential backoff with cap,
   `OnError(ProviderError)`; `WithStale(maxAge)` escalates staleness to hard error.
6. Provider watch channel death: re-establish with backoff; fall back to polling
   after N failures; emit metric.

All time flows through `mamori.Clock` for deterministic tests.

---

## 9. Middleware (`mamori/middleware`)

Each wraps a `Provider` and preserves `Watchable`/`Batch` when the inner supports it:
`Cache(ttl)`, `Audit(logger)`, `Failover(primary, replicas...)`, `RateLimit(rps)`,
`Prefix(prefix)` (multi-tenant namespace rewrite). Composable:

```go
mamori.WithProvider(middleware.Cache(5*time.Minute,
    middleware.Audit(log, middleware.Failover(primary, replica))))
```

---

## 10. Conformance kit (`mamori/providertest`)

```go
providertest.Run(t, providertest.Config{
    New:    func() mamori.Provider { ... },
    Seed:   func(ctx, key, val) error { ... },
    Mutate: func(ctx, key, val) error { ... },
})
```

Exercises: context cancellation, concurrent `Resolve`, `errors.Is(err,
mamori.ErrNotFound)`, `Version` monotonicity, `Watch` closure on ctx cancel,
no goroutine leaks (`goleak`), no payload logging (log-capture assertion). Passing
providers earn a registry badge.

---

## 11. reconcilevet (`tools/reconcilevet`)

`analysis.Analyzer` flagging struct fields with a `source:` tag pointing at a
secret-bearing scheme (`aws-sm`, `gcp-sm`, `azure-kv`, `vault`, `op`, `sops`,
`k8s-secret`) whose Go type is plain `string`/`[]byte` instead of
`secret.String`/`secret.Bytes`. Shipped as a binary; usable via
`go vet -vettool=$(which reconcilevet)`.

---

## 12. Failure model

- **Startup**: `Load`/`Watch` fail fast; never returns partial config.
- **Runtime resolve failure**: last-good retained; `OnError(ProviderError{Scheme,
  Ref, Err})`; per-ref exponential backoff with cap; optional `WithStale(maxAge)`.
- **Validation failure on update**: rejected atomically (§7).
- **Watch channel death**: re-establish with backoff, fall back to poll after N
  failures, emit metric.

## 13. Security

- Sensitive `Value`s never pass through `fmt`/logs unredacted; `reconcilevet`
  flags sensitive-ref → non-secret-field assignments.
- `Zero()` best-effort, GC caveats documented honestly.
- `exec:` disabled by default; refs never interpolated from other resolved values
  (no injection chains).
- Providers must not log ref payloads; kit includes a log-capture assertion.
- Supply chain: minimal core deps; provider modules isolate SDK blast radius;
  releases signed + SLSA provenance via GoReleaser.

---

## 14. Docs site (`site/`, Astro)

**Aesthetic: Guardian / Japanese-modern.** `mamori` = protection. Motifs: an
*omamori* charm mark as logo, seigaiha (wave) background texture, generous *ma*
negative space, dark-first with an indigo/vermilion accent. Hero animation: a
secret value rotates at its source → the typed struct re-validates → the app's
`OnChange` callback fires (pool rotates), all "without restart". Sections:
concepts, quickstart, provider gallery (with conformance badges), middleware,
conformance kit, security, links to pkg.go.dev. Deployed to GitHub Pages via
Actions. Built with the `frontend-design` skill for the "wow".

## 15. CI / release

- `ci.yml`: matrix over all modules — `go test ./...`, `go vet`, race detector,
  `golangci-lint`; runs `reconcilevet` on the examples.
- `pages.yml`: build Astro site, deploy to GitHub Pages.
- `release.yml` + `.goreleaser.yaml`: build `reconcilevet` binary for
  linux/darwin/windows × amd64/arm64, changelog, checksums, SLSA provenance,
  GitHub Release. Library modules release via semver git tags (`v0.1.0` core,
  `providers/aws/v0.1.0` for submodules) — tagging scheme documented.

---

## 16. Testing strategy

- **Core**: exhaustive unit tests using a controllable `Clock` and in-memory fake
  providers — every §7 semantic rule has a dedicated test. Race detector on.
- **Providers**: unit tests against mocked SDK client interfaces; each runs the
  `providertest` kit against an in-memory fake backend. Live-backend integration
  tests written behind `//go:build integration` (localstack / vault dev / envtest)
  but **not executed in this build** — READMEs state clearly what is verified vs
  needs a live run.

## 17. Out of scope (unchanged non-goals)

Not a secrets store, not a sync engine, not a feature-flag system, no cross-language
support. `runtimevar` bridge deferred (avoid muddying the provider story at launch).

---

## 18. Build order

1. **Core** (engine, secret, ref/registry, decode, env/file/exec, providertest,
   middleware) — locks the SPI. Sequential, test-first.
2. **Provider modules** (fan out in parallel once SPI is frozen) + `x/otel` +
   `reconcilevet`.
3. **CI / GoReleaser / Pages** wiring + `go.work`.
4. **Astro docs site** (frontend-design skill).
