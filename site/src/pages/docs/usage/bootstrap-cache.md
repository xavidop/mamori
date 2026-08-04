---
layout: ../../../layouts/DocsLayout.astro
title: Bootstrap cache
---

# Bootstrap cache

A process that is already running survives a backend outage: an update that fails is rejected and `Get()` keeps serving the last valid config. A process that *restarts* during one cannot boot at all, even when the configuration has not changed in weeks.

`WithBootstrapCache` closes that gap. It keeps an encrypted snapshot of the last known-good resolved values on disk, and boots from it when a cold start cannot reach the backend.

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithBootstrapCache("/var/lib/myapp/mamori.snap", key,
		mamori.BootstrapMaxAge(6*time.Hour)),
)
```

| | |
| --- | --- |
| Option | `WithBootstrapCache(path string, key []byte, opts ...BootstrapOption) Option` |
| Sub-option | `BootstrapMaxAge(d time.Duration) BootstrapOption`, default 24h |
| Key | exactly 32 bytes, AES-256-GCM |
| File | `nonce ‖ ciphertext`, mode `0600`, written atomically |
| Applies to | `Load`, `Watch`, and `Doctor` (which inspects the file without restoring it) |

## The trade you are making

**Enabling this creates a file holding live credentials at rest that did not exist before.** It is encrypted with AES-256-GCM and written `0600`, but the honest framing is that you are trading a startup failure for an artifact an attacker with disk access *and* the key could read. Decide that deliberately; it is not free.

## Supplying the key

The key is 32 bytes and mamori does not source it for you. That is the point: the whole feature exists for the moment mamori's own backends are unreachable, so a key fetched through mamori would be unavailable exactly when it is needed.

Read it from the process environment, an instance metadata call, a mounted file, or a KMS SDK you call directly:

```go
key, err := base64.StdEncoding.DecodeString(os.Getenv("MAMORI_BOOTSTRAP_KEY"))
if err != nil {
	log.Fatal(err)
}
```

A key of any length other than 32 fails `Load` and `Watch` immediately, before a single provider round trip, rather than at the first write. Whatever you use must be the *same* key across restarts, and must not live on the same disk as the snapshot.

## The rules

### It is a fallback, never a fast path

Every start resolves normally first. The snapshot is read only if that fails. A healthy process never serves from disk, so the cache can never mask a backend that is up but returning something wrong.

### Only a transient failure falls back

| Failure kind | What happens |
| --- | --- |
| `unavailable`, `rate_limited` | the snapshot is restored |
| `not_found`, `permission_denied`, `unauthenticated`, `invalid`, `unknown` | the start **fails** |

The backend being unreachable means the cached value is the best available answer. The backend *answering* and saying no means something changed: a secret was deleted, a credential was revoked, a policy was tightened. Serving a cached copy of a secret someone deliberately removed would undo the removal, so those fail the start.

`unknown` is on the failing side too. A provider that cannot classify its own error has not established that the backend was unreachable, and guessing in the permissive direction is guessing about a revocation.

### An expired lease is not restorable

If a provider set `Value.NotAfter` (Vault fills it from the lease) and that instant has passed, the record is refused and the start fails with an error naming the field. Restoring it would hand your application a credential the backend has already invalidated, while reporting a healthy boot.

### The snapshot is written on every applied update

The file tracks the configuration this process is actually serving. It is written after a candidate has passed decoding, every `WithDerive` hook, validation, and the `PreApply` gate, so it can never hold a value the process itself rejected. A restore does **not** rewrite it: rewriting the file with what was just read from it would reset its age and hand back the bound `BootstrapMaxAge` exists to enforce.

### A write failure never fails the update

If the snapshot cannot be written, the configuration is applied anyway. It resolved, validated, and passed the gate; refusing it because a cache file could not be written would turn a fallback meant to survive an outage into a new way to fail during one.

You still find out. The failure goes to `OnError`, to the [logger](/docs/telemetry/logging/), and to the `mamori_bootstrap_write_failed_total` / `mamori.bootstrap.write.failed.count` counter. Alert on that counter: nothing else breaks at the moment it happens, and the damage shows up later, at a restart that finds no fallback.

The counter reaches your metrics sink through `mamori.BootstrapMeter`, an optional interface that adds `RecordBootstrapWriteFailed()` to `mamori.Meter`. [`x/otel`](/docs/telemetry/opentelemetry/) and [`x/prom`](/docs/telemetry/prometheus/) implement it, so with either bridge there is nothing to do. If you wrote your own `Meter`, add the method or this one event passes it by; `OnError` and the log line still fire either way.

### A restored config is still validated

The snapshot stores *resolved values*, not the decoded struct, and a restore replays them through the ordinary decode, derive, validate and `PreApply` path. If any of those reject the restored configuration, the start fails, and the error names both the outage and the rejection.

Storing resolved values rather than the struct is not an implementation detail you can ignore: `secret.String` marshals to `[REDACTED]` by design, so a snapshot of `T` would faithfully persist the redaction and silently lose every secret in your config.

## Health while serving from the snapshot

`Health()` **passes** while a restored configuration is within `BootstrapMaxAge`, so the pod joins the load balancer and a backend outage does not also become a total outage. Past that bound it returns a `*BootstrapStaleError` and the pod drops out of rotation, because a configuration frozen for longer than you said you would tolerate is not one worth serving.

```go
if err := w.Health(); err != nil {
	var stale *mamori.BootstrapStaleError
	if errors.As(err, &stale) {
		log.Printf("serving a %s-old snapshot, past the %s bound", stale.Age, stale.MaxAge)
	}
}
```

**Set `BootstrapMaxAge` to the rotation window of the shortest-lived credential in your config.** A process serving credentials older than that will fail against the backend that rotated them, and failing readiness is the better outcome. `BootstrapMaxAge(0)` means unbounded; you have to write it, because a configuration that is stale forever and silent about it is not a default anyone should get by accident.

A negative duration is refused: `Load` and `Watch` return an error wrapping `ErrInvalid` before anything resolves, the same as a wrong-sized key. It is not quietly clamped, because both ways of clamping it are worse than the error. Rounding up to zero would turn a sign typo into the unbounded mode you are supposed to write out; rounding the other way would expire every snapshot on sight and drop the pod from rotation during the outage the cache exists for.

Once every field has been answered by its own backend, the bound stops applying: a pod that booted during a two-minute outage and has been reconciling normally for twenty hours is no longer serving anything the snapshot decided, and would otherwise fail its own readiness probe over a file it no longer reads.

## Seeing it in Status, the admin endpoint, and doctor

`Report` gains two fields, so "we booted off disk" is visible from the first second rather than only once it turns unhealthy.

```go
rep := w.Status()
if rep.Source == mamori.SourceBootstrapCache {
	log.Printf("serving a snapshot written %s ago", rep.Bootstrap.Age)
}
```

`Source` is `backend` for every ordinary process and `bootstrap_cache` only while the snapshot is still deciding what is served. `Bootstrap` carries whether a snapshot exists, when it was written, how old it is, whether it still fits this build's config struct, and whether this process booted from it. See [Observability](/docs/observability/#the-bootstrap-cache-block) for the full shape.

The same block reaches the [admin endpoint](/docs/observability/admin/) and `mamori status` / `mamori doctor`:

```text
HEALTHY: 4 field(s), snapshot 1 (live 1), generated 2026-08-04T09:12:33Z
BOOTSTRAP CACHE: serving a snapshot written 2h0m0s ago; the backend has not been reached for every field since this process started
```

After the backend comes back and every field resolves live again, `Source` returns to `backend` and the shouting line becomes a quiet one that still records how this process started:

```text
HEALTHY: 4 field(s), snapshot 2 (live 2), generated 2026-08-04T11:41:07Z
bootstrap cache: this process booted from a snapshot and has since resolved every field live; snapshot written 3m0s ago, matching this config
```

That is what makes a post-mortem possible an hour later. A process that never restored says only `bootstrap cache: snapshot written 3m0s ago, matching this config`.

Neither the snapshot's contents nor its path ever appear there. A `Report` is designed to be served over HTTP, and telling an unauthenticated reader where the encrypted credential file lives buys them a step for nothing in return.

## Gotchas

**Changing your config struct invalidates the snapshot.** The file records a fingerprint of the field paths, refs, types, and sensitivity it was written for. Add, remove, rename, or retype a field and an older snapshot is refused with an error naming schema drift, rather than failing later with a confusing message about a value. Reordering fields is not drift. Run `mamori doctor` after a config change to see whether the fallback you think you have still matches.

**The age is time since the config last *changed*, not since it was last confirmed.** The snapshot is rewritten on every applied update, so a configuration that genuinely never changes ages without bound and will eventually exceed `BootstrapMaxAge`. If nothing in your config rotates, `BootstrapMaxAge(0)` is the honest setting.

**The snapshot must survive the restart.** A file in a container's writable layer is gone when the container is replaced, which is the restart this feature exists for. Put it on a volume that outlives the process: a Kubernetes `emptyDir` survives a container restart but not a pod reschedule; a PVC or a host path survives both.

**It is per-process, not shared.** Two processes pointed at the same path will overwrite each other's snapshots. Give each replica its own path (a subdirectory per pod), or accept that the file reflects whichever wrote last.

**A `PreApply` gate still runs on a restored config.** If your gate opens a connection to prove a credential works, and the outage that stopped the config backend also reached that dependency, the start fails. That is correct, and it is worth knowing before you first see it.

**Rotating the key invalidates every existing snapshot.** A snapshot sealed with the old key does not authenticate under the new one and is refused. Plan the rollout so at least one normal boot writes a fresh snapshot under the new key before you rely on the fallback again.

## See also

- [Options reference](/docs/usage/options/) for `WithBootstrapCache` alongside every other option.
- [Observability](/docs/observability/) for `Report.Source`, `BootstrapStatus`, and the `Health` rules.
- [Doctor](/docs/observability/doctor/) for checking the snapshot before the outage rather than during it.
- [Snapshots and pinning](/docs/usage/snapshots/) for the in-memory snapshot history, which is a different thing entirely: it holds past configurations for `Pin`, and dies with the process.
