---
layout: ../../../layouts/DocsLayout.astro
title: "Doctor: pre-deploy check"
---

# Doctor: pre-deploy check

```go
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error)
```

`Doctor` resolves every field of `T` exactly once and returns a [`Report`](/docs/observability/#report-and-fieldstatus) describing what succeeded and what failed, without starting a watcher. It accepts the same `Option`s as `Load` and `Watch`, so it exercises your real provider wiring, middleware, and `Prefix` rewriting.

Unlike `Load`, `Doctor` never aborts on the first failure: it walks every field and records each result, so one run reports every misconfigured ref instead of just the first. The returned `error` is non-nil only when `T` itself cannot be walked as a config struct (an unsupported field type, for example); individual field failures live in the `Report`, not the returned error. `Doctor` does not decode or validate values.

`Report.Snapshot` and `Report.Live` are always `0` for a `Doctor` report (and `Report.Pinned` is always `false`), marking a one-shot probe rather than a running watcher's snapshot (whose version starts at 1).

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
