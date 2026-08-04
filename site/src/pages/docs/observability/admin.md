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

**`WithRefVars` values must not be secrets.** After [`${VAR}` interpolation](/docs/concepts/ref-interpolation/) expands a ref, that ref's `Raw` holds the expanded string - and this endpoint's `Report` is exactly where it becomes visible, alongside `Status()` and `mamori doctor` output. Variables are for environment names, regions, service names, and tenant identifiers, not for anything that itself needs to stay confidential.

## Telling a live pod from one that booted on a snapshot

With the [bootstrap cache](/docs/usage/bootstrap-cache/) configured, `GET /` answers the question you actually have during an incident: is this pod serving what its backends said, or what a file on its disk held?

```json
{
  "Source": "bootstrap_cache",
  "Bootstrap": {
    "Present": true,
    "Restored": true,
    "WrittenAt": "2026-08-04T17:47:40Z",
    "Age": 7200000000000,
    "FingerprintMatch": true,
    "Problem": ""
  },
  "Healthy": true,
  "Snapshot": 1,
  "Live": 1
}
```

**Alert on `Source`.** It reads `bootstrap_cache` only while the snapshot is still covering for at least one field, and returns to `backend` on its own once every field has been resolved live, so the alert clears when the outage does.

**Check `Restored` afterwards.** It stays `true` for the whole life of a pod that booted from disk, which is how you tell an hour later that this pod restarted *during* the outage rather than after it. `Age` is the snapshot's age in nanoseconds and `WrittenAt` the instant it was written, so "how old is the config this pod is serving" is one field. `Problem` names the reason when a snapshot exists but could not be used, a wrong key being the common one, and `FingerprintMatch` is `false` when the snapshot was written by a build whose config struct no longer matches this one.

`Healthy` describes fields, not the snapshot, so it stays `true` while serving a restored config. That is deliberate: the pod joins the load balancer instead of the outage becoming total. What drops it out once the snapshot passes `BootstrapMaxAge` is `GET /healthz`, which returns `503` for a config frozen longer than you allowed.

On a process that does not configure the cache both keys are absent from the body entirely, so anything already parsing this endpoint sees exactly what it saw before.

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

## No `POST /refresh`

There is no route here that triggers a reload, and there will not be one. `GET /` and `GET /healthz` are the whole surface - every other path and method is `404` - and both only ever read `w.Status()`, never write anything. That is a deliberate security property, not a missing feature: this endpoint exists to report on a watcher that already handles secret material, and a mutating route on it would let anyone who can merely *observe* that material also *trigger* a fresh resolve of it, on demand. Read access and reload access are different privileges, and this surface only ever grants the first.

If you want an HTTP-triggered refresh, mount one yourself on the same `mux`, gated by whatever authorization you already trust for an administrative action - it does not have to be, and generally should not be, the same `Authenticator` guarding the read-only `Report` above:

```go
mux.HandleFunc("/refresh", func(rw http.ResponseWriter, r *http.Request) {
	if !authorizedForReload(r) { // your own check, not mamori's
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}
	if err := w.Refresh(r.Context()); err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
})
```

`w.Refresh` itself - what it does, why it blocks, and what it returns - is covered in [Rotation safety](/docs/usage/refresh/).

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
- [Rotation safety](/docs/usage/rotation/) - `PreApply` and `w.Refresh`, which a hand-rolled `/refresh` route above would call.
- [Config server](/docs/server/) - serves resolved config *values*, not metadata.
- [Auth](/docs/auth/) - `WithAuth`, the shipped schemes, and credential rotation.
- [Bootstrap cache](/docs/usage/bootstrap-cache/) - the option behind the `Source` and `Bootstrap` fields above.
