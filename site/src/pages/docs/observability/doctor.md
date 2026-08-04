---
layout: ../../../layouts/DocsLayout.astro
title: "Doctor: pre-deploy check"
---

# Doctor: pre-deploy check

```go
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error)
```

`Doctor` resolves every field of `T` exactly once and returns a [`Report`](/docs/observability/#report-and-fieldstatus) describing what succeeded and what failed, without starting a watcher. It accepts the same `Option`s as `Load` and `Watch`, so it exercises your real provider wiring, middleware, and `Prefix` rewriting.

Unlike `Load`, `Doctor` never aborts on the first failure: it walks every field a `source` tag declares and records each result, so one run reports every misconfigured ref instead of just the first. The returned `error` is non-nil only when `T` itself cannot be walked as a config struct (an unsupported field type, for example); individual field failures live in the `Report`, not the returned error. `Doctor` never validates values and never hands back a populated `T`.

`Report.Snapshot` and `Report.Live` are always `0` for a `Doctor` report (and `Report.Pinned` is always `false`), marking a one-shot probe rather than a running watcher's snapshot (whose version starts at 1).

## Derived fields are probed

`Doctor` also runs your [`WithDerive`](/docs/usage/derived-fields/) hooks against the values it just resolved, and adds one [`FieldStatus`](/docs/observability/#report-and-fieldstatus) per declared write path. Four outcomes:

| Outcome | The row | Healthy? |
| --- | --- | --- |
| Hook succeeded | `Version` is a content hash of the value | yes |
| Hook returned an error | `Version` blank, `LastKind` `invalid`, `LastError` is the hook's error | no |
| An input produced no value | hook never ran, `Version` blank, `LastError` says not evaluated, no `LastKind` | unchanged |
| Hooks cannot be typed to `T` | one `invalid` row per declared path | no |

A field counts as having produced a value if it resolved, fell back to its `default` (including via `onfail:"default"`), or is an absent `optional`. Anything else blocks the hooks, including `unavailable`, `rate_limited`, and `unknown`, which leave the report healthy but leave the hook nothing to read. Deriving from a zero value would publish a version that looks real and does not match production.

Blocking is all-or-nothing: mamori cannot tell which fields a hook reads, so one unresolved field blocks every derived row.

`Doctor` executes your hook code, so a hook with a side effect runs for real on every `Doctor` run.

## Your fallback is checked while the backends are still up

If you configure the [bootstrap cache](/docs/usage/bootstrap-cache/), `Doctor` also inspects the snapshot on disk and fills in `Report.Bootstrap`: whether one exists, whether it decrypts with the key this build is configured with, and whether it still fits this build's config struct.

It never restores from it. Every value a `Doctor` report describes came from the backend it just called, so `Report.Source` stays `backend`; the `Bootstrap` block beside it describes the file this process *would* fall back to, not where these values came from.

```go
if bs := rep.Bootstrap; bs != nil && (!bs.Present || !bs.FingerprintMatch) {
	t.Fatalf("no usable bootstrap snapshot (present=%v, matches this build=%v): %s",
		bs.Present, bs.FingerprintMatch, bs.Problem)
}
```

That check earns its place in a preflight because a bootstrap cache is otherwise exercised only during the outage it exists for, which is the worst possible moment to discover the key is wrong or that the snapshot predates your last config-struct change. Both fields are absent on a process that does not configure the cache.

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
- [Bootstrap cache](/docs/usage/bootstrap-cache/) - the snapshot this preflight inspects.
