---
layout: ../../../layouts/DocsLayout.astro
title: Admin endpoint
---

# Admin endpoint

Serve a watcher's [`Report`](/docs/observability/#report-and-fieldstatus) over HTTP two ways: mount `Handler` on a mux you already run, or let mamori run its own server with `WithAdminHTTP`. Both expose exactly the same two routes.

| Route | Response |
| --- | --- |
| `GET /` | The `Report`, as JSON (same shape as `w.Status()`) |
| `GET /healthz` | A liveness/readiness signal, `{"status":"ok"}` or `{"status":"unhealthy",...}` |

Every other path, and every other method, is `404`.

## Metadata only, never a value

**This is a metadata endpoint. It never serves a configuration value, under any option, on any route.** The JSON body is always `w.Status()`, whose `Ref` fields are already redacted and which never carries a resolved value. `Watcher.Pin` / `Watcher.PinCurrent` / `Watcher.Unpin` are not reachable through either route: `GET /` reports `Pinned` (and the `Snapshot`/`Live` divergence it causes) read-only, but nothing here changes it. For the surface that serves resolved config *values* to many callers, see the [config server](/docs/server/).

**`WithRefVars` values must not be secrets.** After [`${VAR}` interpolation](/docs/concepts/ref-grammar/#ref-interpolation-var) expands a ref, that ref's `Raw` holds the expanded string - and this endpoint's `Report` is exactly where it becomes visible, alongside `Status()` and `mamori doctor` output. Variables are for environment names, regions, service names, and tenant identifiers, not for anything that itself needs to stay confidential.

## Mount Handler on your own mux

```go
func Handler[T any](w *Watcher[T], opts ...HandlerOption) http.Handler
```

```go
w, err := mamori.Watch[Config](ctx)
if err != nil {
	log.Fatal(err)
}
defer w.Close()

mux := http.NewServeMux()
mux.Handle("/", mamori.Handler(w))
go http.ListenAndServe(":8080", mux)
```

Mount it under a subpath with `HandlerPrefix`, which strips the prefix before the request reaches mamori's own routing:

```go
mux.Handle("/admin/", mamori.Handler(w, mamori.HandlerPrefix("/admin")))
```

`HandlerMiddleware` wraps the handler with a non-authentication concern such as request logging. It runs outside `HandlerPrefix`'s stripping and outside any `WithAuth` check, in the order the options are given. Authentication itself, `WithAuth` and the shipped schemes, is covered on the [Auth](/docs/auth/) page.

## Run a standalone server with WithAdminHTTP

```go
func WithAdminHTTP(addr string, opts ...HandlerOption) Option
func WithAdminTLS(cfg *tls.Config) Option
```

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithAdminHTTP("127.0.0.1:9090"),
)
if err != nil {
	log.Fatal(err) // includes a bind failure
}
defer w.Close()

log.Printf("admin endpoint listening on %s", w.AdminAddr())
```

`WithAdminHTTP` is for a caller who does not already run a mux of their own. It carries the same fail-fast lifecycle guarantees as the rest of mamori:

- **Off by default.** With no `WithAdminHTTP` option, no listener is bound and no goroutine starts.
- **A bind failure fails `Watch`.** The listener is bound before `Watch` returns, so a port already in use, or a permission error, comes back as `Watch`'s own error.
- **`Close` releases the port.** `Watcher.Close` shuts the admin server down gracefully, bounded by a short grace period, before it returns.
- **`AdminAddr()` gives you the bound address** (`func (w *Watcher[T]) AdminAddr() net.Addr`), which is `nil` unless `WithAdminHTTP` was used. This is how you discover the port the OS actually chose when binding to `:0`.
- **`WithAdminTLS(cfg)` serves the endpoint over TLS** instead of plaintext, and has no effect without `WithAdminHTTP`. Pair it with an `Authenticator` (see [Auth](/docs/auth/)) so a credential sent to the endpoint is never sent in the clear.
- `Load` accepts `WithAdminHTTP` too, since `Load` and `Watch` share the same `Option` type, but `Load` has no long-lived watcher to run a server against, so it silently ignores the option.

## Wire a readiness probe

`GET /healthz` is built to back a Kubernetes readiness probe directly. Start the admin endpoint and point the probe at it:

```go
w, err := mamori.Watch[Config](ctx, mamori.WithAdminHTTP(":9090"))
```

```yaml
readinessProbe:
  httpGet:
    path: /healthz
    port: 9090
  periodSeconds: 5
```

An unauthenticated caller, such as a kubelet probe, always gets a bare status (`200 {"status":"ok"}` or `503 {"status":"unhealthy"}`), so readiness never depends on holding a credential, even when the endpoint has [`WithAuth`](/docs/auth/) configured. `/healthz` never returns `401`. If auth is configured and the caller authenticates (or no auth is configured at all), the body also includes the failing-field detail (the same fields a `*HealthError` carries). The response body is always metadata, never a config value.

## See also

- [Observability overview](/docs/observability/) - `Status`, `Health`, and the `Report` shape.
- [Doctor](/docs/observability/doctor/) - the pre-deploy counterpart to these live endpoints.
- [Config server](/docs/server/) - serves resolved config *values*, not metadata.
- [Auth](/docs/auth/) - `WithAuth`, the shipped schemes, and credential rotation.
