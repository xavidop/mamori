# Bootstrap cache: surviving a cold start during a backend outage

**Goal:** a process that restarts while its configuration backend is unreachable boots from an encrypted on-disk snapshot of the last known-good values, instead of failing to start.

**Status:** design approved, pending spec review.

**Programme:** PR9 of the sixteen in
`2026-08-03-provider-and-core-expansion-design.md`. Independent of PR1 through PR8.

## The gap

A process that is already running survives a backend outage: an update that fails
validation is rejected and `Get()` keeps serving the last valid config. A process that
*restarts* during one cannot boot at all, even when the configuration has not changed
in weeks. `Watch` does a fail-fast `loadValue` (`reconciler.go:427`) and returns the
error.

Nothing in the project closes this. `middleware.Cache` is memory-only, so it dies with
the process. `WithStale` bounds how old a value may be while running; it does not help
a process that has no value yet. Vault Agent and External Secrets Operator both solve
this, and mamori's own pitch is that it keeps serving through exactly this failure.

## What is cached, and why it cannot be `T`

The snapshot holds **resolved values, not the decoded struct**.

`secret.String.MarshalJSON` returns `"[REDACTED]"` by design (`secret/secret.go:44`),
and `LogValue`, `String` and `GoString` do the same. Serialising `T` would faithfully
persist the redaction and silently lose every secret. That is not a defect to work
around; it is the type doing its job.

So the cache stores what `loadValue` resolved *before* decoding: for each field spec,
the winning ref and its `Value` (bytes, version, sensitive flag). On a cold start it
replays those through the ordinary decode, derive and validate path. Validation
therefore still runs against a restored snapshot, which is the property that makes
this safe rather than a way to smuggle an invalid config past the gate.

## Semantics

**It is a fallback, never a fast path.** Every boot resolves normally first. The
snapshot is read only when that fails, so a healthy process never serves from disk and
the cache cannot mask a backend that is up but wrong.

**Only a transient failure falls back.** If the failure kind is `KindUnavailable` or
`KindRateLimited`, the backend could not be reached and the cached value is the best
available answer. If it is `KindNotFound`, `KindPermissionDenied`,
`KindUnauthenticated` or `KindInvalid`, the backend answered and said no: a secret was
deleted, a credential was revoked, a policy changed. Serving a cached copy of a secret
the backend has deliberately removed would defeat the revocation. Those fail the boot.

**Written on every applied update.** The snapshot tracks the last config that passed
validation and `PreApply`, so it can never hold a value the process itself rejected.

**An expired lease is not restorable.** `Value.NotAfter` carries the instant a value is
known to expire, which Vault populates from the lease. A cached value whose `NotAfter`
has already passed names a credential the backend has itself invalidated, so restoring
it would hand the application something guaranteed not to work, and would do it while
reporting a healthy boot. Such a record is refused. If every record is still live the
boot proceeds; if any required one has expired, the boot fails with an error naming the
field, because the honest answer is that this process cannot start without reaching the
backend.

**Health is bounded.** `Health()` passes while serving from the snapshot, so the pod
joins the load balancer and the outage does not become total. Past `maxAge` it fails,
so a config frozen longer than the operator tolerates takes the pod out rather than
serving indefinitely stale secrets. This mirrors `WithStale`'s existing precedent.

**Reported loudly throughout.** `Report` gains the snapshot's source and age, so
"serving from a two-hour-old snapshot" is visible in `Status()`, the admin endpoint and
`mamori doctor` from the first second, not only once it turns unhealthy.

## API

```go
func WithBootstrapCache(path string, key []byte, opts ...BootstrapOption) Option
func BootstrapMaxAge(d time.Duration) BootstrapOption
```

`key` is 32 bytes, AES-256-GCM. A key of any other length is rejected at construction.
Where the key comes from is a deployment concern: an environment variable, an instance
metadata call, a file. mamori does not source it, because the whole point is that
mamori's own backends are unreachable at the moment it is needed.

`maxAge` defaults to 24 hours. Zero means unbounded, which must be written explicitly.

## On-disk format

```
nonce (12 bytes) || AES-256-GCM ciphertext
```

The plaintext is a versioned JSON document holding a format version, the write time, a
schema fingerprint, and one record per field: its ref, bytes, version string and
sensitive flag. Written to a temporary file in the same directory and renamed over the
target, so a crash mid-write leaves the previous snapshot intact rather than a
truncated one. Mode `0600`.

**Schema fingerprint.** A hash of the field specs. If `T` gains a field, an older
snapshot cannot satisfy it, and replaying it would fail validation with a confusing
message about a field the snapshot never knew about. The fingerprint turns that into
one clear error naming schema drift as the cause. It is a correctness guard, not a
security one.

**A corrupt snapshot never masks the real failure.** If decryption or parsing fails,
the returned error wraps both the original resolve failure and the cache failure. An
operator debugging a failed boot needs to know the backend was down *and* that the
snapshot they were relying on is unusable.

## Documentation

Per the standing rule: `site/src/pages/docs/usage/bootstrap-cache.md` and a sidebar
entry, the root `README.md` feature bullet, `site/src/pages/docs/usage/options.md`,
`site/src/pages/docs/observability/index.md` for the new `Report` fields, and
`skills/mamori/SKILL.md`.

## Out of scope

Sourcing the key. Any cloud KMS integration; a `Sealer` interface can be added later if
a second consumer appears, following this project's rule that an abstraction lands with
its second consumer rather than ahead of it.

Sharing one snapshot between processes. It is a per-process cold-start aid, not a
distributed cache.

## Risks

**A snapshot is a secret at rest that did not exist before.** Encryption and `0600` are
the mitigation, but the honest framing is that this feature trades a startup failure
for a new artifact holding live credentials. The documentation must say so plainly
rather than presenting the feature as free.

**Stale config is a real hazard, not a theoretical one.** A process serving six-hour-old
credentials after a rotation will fail against the backend that rotated them. The
bounded `maxAge` and the loud `Status()` reporting exist for this, and `maxAge` should
be set to the operator's rotation window rather than left at the default.
