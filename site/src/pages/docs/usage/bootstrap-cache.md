---
layout: ../../../layouts/DocsLayout.astro
title: Bootstrap cache
---

# Bootstrap cache

A process that is already running survives a backend outage: an update that fails is rejected and `Get()` keeps serving the last valid config. A process that *restarts* during one cannot boot at all.

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

mamori does not source the key for you. The whole feature exists for the moment mamori's own backends are unreachable, so a key fetched through mamori would be unavailable exactly when it is needed. Read it from the process environment, an instance metadata call, a mounted file, or a KMS SDK you call directly:

```go
key, err := base64.StdEncoding.DecodeString(os.Getenv("MAMORI_BOOTSTRAP_KEY"))
if err != nil {
	log.Fatal(err)
}
```

A key of any length other than 32 fails `Load` and `Watch` immediately, before a single provider round trip. Use the *same* key across restarts, and keep it off the disk holding the snapshot.

## When it restores, and when it refuses

Every start resolves normally first. The snapshot is read only if that fails, and only for these failure kinds:

| Failure kind | What happens |
| --- | --- |
| `unavailable`, `rate_limited` | the snapshot is restored |
| `not_found`, `permission_denied`, `unauthenticated`, `invalid`, `unknown` | the start **fails** |

An unreachable backend falls back; a backend that answers and says no does not.

A restore that does begin is still refused, failing the start, when:

- a provider set `Value.NotAfter` (Vault fills it from the lease) and that instant has passed. The error names the field.
- the snapshot was written for a different config struct. See [Gotchas](#gotchas) below.
- the restored values fail the ordinary decode, derive, validate and `PreApply` path they are replayed through. The error names both the outage and the rejection.

## When it is written

The snapshot is rewritten on every applied update, after decoding, every `WithDerive` hook, validation, and the `PreApply` gate. A restore does **not** rewrite it.

A write failure never fails the update. The configuration is applied, and the failure goes to `OnError`, to the [logger](/docs/telemetry/logging/), and to the `mamori_bootstrap_write_failed_total` / `mamori.bootstrap.write.failed.count` counter. **Alert on that counter.** Nothing else breaks at the moment it fires; the damage shows up later, at a restart that finds no fallback.

[`x/otel`](/docs/telemetry/opentelemetry/) and [`x/prom`](/docs/telemetry/prometheus/) record it already. A hand-written `mamori.Meter` has to add `RecordBootstrapWriteFailed()` to see it; `OnError` and the log line fire either way.

## Health while serving from the snapshot

`Health()` **passes** while a restored configuration is within `BootstrapMaxAge`, so the pod joins the load balancer and a backend outage does not also become a total outage. Past that bound it returns a `*BootstrapStaleError` and the pod drops out of rotation.

```go
if err := w.Health(); err != nil {
	var stale *mamori.BootstrapStaleError
	if errors.As(err, &stale) {
		log.Printf("serving a %s-old snapshot, past the %s bound", stale.Age, stale.MaxAge)
	}
}
```

**Set `BootstrapMaxAge` to the rotation window of the shortest-lived credential in your config.** `BootstrapMaxAge(0)` means unbounded and has to be written explicitly. A negative duration is refused with an error wrapping `ErrInvalid`, before anything resolves, rather than clamped.

The bound stops applying once every field has been answered by its own backend, so a pod that booted during a two-minute outage and has reconciled normally for twenty hours is not failing readiness over a file it no longer reads.

## Seeing it in Status, the admin endpoint, and doctor

```go
rep := w.Status()
if rep.Source == mamori.SourceBootstrapCache {
	log.Printf("serving a snapshot written %s ago", rep.Bootstrap.Age)
}
```

`Source` is `backend` for every ordinary process, and `bootstrap_cache` only while the snapshot is still deciding what is served. `Bootstrap` carries whether a snapshot exists, when it was written, how old it is, whether it still fits this build's config struct, and whether this process booted from it. See [Observability](/docs/observability/#the-bootstrap-cache-block) for the full shape.

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

A process that never restored says only `bootstrap cache: snapshot written 3m0s ago, matching this config`. Neither the snapshot's contents nor its path ever appear in any of them.

## Gotchas

**Changing your config struct invalidates the snapshot.** The file records a fingerprint of the field paths, refs, types, and sensitivity it was written for. Add, remove, rename, or retype a field and an older snapshot is refused with an error naming schema drift. Reordering fields is not drift. Run `mamori doctor` after a config change to see whether the fallback you think you have still matches.

**The age is time since the config last *changed*, not since it was last confirmed.** The snapshot is rewritten on every applied update, so a configuration that genuinely never changes ages without bound and will eventually exceed `BootstrapMaxAge`. If nothing in your config rotates, `BootstrapMaxAge(0)` is the honest setting.

**The snapshot must survive the restart.** A file in a container's writable layer is gone when the container is replaced, which is the restart this feature exists for. A Kubernetes `emptyDir` survives a container restart but not a pod reschedule; a PVC or a host path survives both.

**It is per-process, not shared.** Two processes pointed at the same path overwrite each other's snapshots. Give each replica its own path, a subdirectory per pod.

**A `PreApply` gate still runs on a restored config.** If your gate opens a connection to prove a credential works, and the outage that stopped the config backend also reached that dependency, the start fails.

**Rotating the key invalidates every existing snapshot.** A snapshot sealed with the old key does not authenticate under the new one and is refused. Plan the rollout so at least one normal boot writes a fresh snapshot under the new key before you rely on the fallback again.

## See also

- [Options reference](/docs/usage/options/) for `WithBootstrapCache` alongside every other option.
- [Observability](/docs/observability/) for `Report.Source`, `BootstrapStatus`, and the `Health` rules.
- [Doctor](/docs/observability/doctor/) for checking the snapshot before the outage rather than during it.
- [Snapshots and pinning](/docs/usage/snapshots/) for the in-memory snapshot history, which is a different thing entirely: it holds past configurations for `Pin`, and dies with the process.
