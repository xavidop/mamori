---
layout: ../../../layouts/DocsLayout.astro
title: "Doctor: pre-deploy check"
---

# Doctor: pre-deploy check

```go
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error)
```

`Doctor` resolves every field of `T` exactly once and returns a [`Report`](/docs/observability/#report-and-fieldstatus) describing what succeeded and what failed, without starting a watcher. It accepts the same `Option`s as `Load` and `Watch`, so it exercises your real provider wiring, middleware, and `Prefix` rewriting.

Unlike `Load`, `Doctor` never aborts on the first failure: it walks every field a `source` tag declares and records each result, so one run reports every misconfigured ref instead of just the first. The returned `error` is non-nil only when `T` itself cannot be walked as a config struct (an unsupported field type, for example); individual field failures live in the `Report`, not the returned error. `Doctor` does not decode or validate values.

`Report.Snapshot` and `Report.Live` are always `0` for a `Doctor` report (and `Report.Pinned` is always `false`), marking a one-shot probe rather than a running watcher's snapshot (whose version starts at 1).

## Derived fields are probed

"Every field," above, now also means every field a [`WithDerive`](/docs/usage/derived-fields/) hook declares writing, not only the ones a `source` tag declares. `Doctor` runs the registered hooks against the values it just resolved and appends one [`FieldStatus`](/docs/observability/#report-and-fieldstatus) per declared write path, each carrying `Derived: true`. There are exactly four outcomes for a derived row:

- **Every sourced field produced a value and the hook succeeded**: `Version` is a content hash of the value the hook produced - the same kind of version a provider without a native revision already reports.
- **Every sourced field produced a value but the hook returned an error**: `Version` stays blank, `LastKind` reads `invalid`, `LastError` carries the hook's own error, and the report is unhealthy.
- **A sourced field produced no value**: the hook never runs at all, so there is no value to hash. `Version` stays blank and `LastError` says the field was not evaluated, with no `LastKind` - there was nothing to classify, because nothing ran.
- **The hooks cannot be typed to `T`**: a hook written for a different config, or one declaring an empty write path, fails `Load` and `Watch` outright with `ErrInvalid`. `Doctor` keeps its contract that the returned `error` covers only an unwalkable `T`, so it reports the rejection as one row per declared write path instead, each carrying `LastKind: invalid` and the rejection's text, and the report is unhealthy. A preflight reporting green on a config that cannot start is the one outcome it must never produce.

"Produced no value" is decided by what each field ended up holding, not by whether the report came back healthy. A field counts as having produced a value when it resolved, when it fell back to its `default` (on absence, or on an error it opted into tolerating with `onfail:"default"` - exactly as `Load` would), or when it is `optional` and absent, which leaves it at the zero value `Load` leaves it at too. Anything else blocks the hooks, including a field failing with `unavailable`, `rate_limited`, or `unknown`: those kinds are self-healing, so they leave the report healthy while still leaving the hook nothing to read. Running it anyway against a zero value would publish a version that looks real and does not match what production computes, which is worse than no version at all.

That blocking is all-or-nothing across every derived field, not only the ones whose own inputs failed: `Doctor` cannot inspect a hook's closure to learn which fields it reads, so a single unresolved sourced field blocks every derived row, not just the ones that depend on it.

Note that `Doctor` executes your hook code. A hook with a side effect, such as a metric increment or a call to another service, runs for real on every `Doctor` run, not only when a config is about to ship.

## Run it before a watcher starts

```go
rep, err := mamori.Doctor[Config](ctx, appProviders()...)
if err != nil {
	log.Fatal(err) // T is not a walkable config struct
}
for _, f := range rep.Fields {
	if f.LastKind != "" {
		log.Printf("unreachable: %s (%s): %s: %s", f.Path, f.Ref, f.LastKind, f.LastError)
	}
}
if !rep.Healthy {
	log.Fatal("config is not deployable")
}
```

## CI preflight

Run `Doctor` as a build-tagged test, gated behind a tag so it only runs where real credentials and network access are available, and fail the build on any field that did not come back healthy:

```go
//go:build preflight

func TestConfigPreflight(t *testing.T) {
	rep, err := mamori.Doctor[Config](context.Background(), appProviders()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Fields {
		if f.LastKind != "" {
			t.Errorf("%s (%s): %s: %s", f.Path, f.Ref, f.LastKind, f.LastError)
		}
	}
}
```

```bash
go test -tags preflight ./...
```

That catches a rotated-away secret, a missing IAM permission, or a typo'd ref before it ships, instead of at container startup.

## See also

- [Observability overview](/docs/observability/) - `Status` and `Health` for a running watcher, and the `Report` shape.
- [HTTP exposure](/docs/observability/admin/) - serve the same `Report` over HTTP.
- [Error kinds](/docs/concepts/error-kinds/) - what each `LastKind` value means.
