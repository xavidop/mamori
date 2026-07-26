---
layout: ../../../layouts/DocsLayout.astro
title: Server wire protocol
---

# Server wire protocol

The handler `Handler()` returns (and `Serve` mounts on every transport) exposes five v1 routes. This is a versioned wire protocol, not an internal detail: once an external client depends on today's routes and JSON fields, they are a stable contract. Every value-bearing response carries `Cache-Control: no-store`.

| Route | Purpose |
| --- | --- |
| `GET /v1/values/{name}` | Resolve one binding by name |
| `POST /v1/values` | Resolve a batch: body `{"names":[...]}` |
| `GET /v1/watch` | Subscribe to one or more bindings over Server-Sent Events |
| `GET /v1/healthz` | Bare liveness check |
| `GET /v1/readyz` | Readiness: should a load balancer route here |

## Request ordering

Every value-bearing route runs, in this exact order, on every request. The two probe routes (`/v1/healthz` and `/v1/readyz`) are exempt:

1. **Authenticate** (`mamori.Authenticator.Authenticate`, or skipped in `NoAuth` mode). Failure is `401`, with `WWW-Authenticate` set if the `Authenticator` implements `Challenger`.
2. **Authorize the specific name** (`Policy.Allow`), separately for every name in a batch or watch subscription. Failure is `403`.
3. **Only then** read the binding.

A denied caller gets `403` for a name whether or not it is a real binding: authorization runs and fails closed before the binding table is consulted. The probe routes are the exception; neither authenticates, authorizes, nor names a binding.

## Response shapes

A successful single value, one entry of a batch response, or one SSE update frame all share one JSON shape:

```json
{
  "name": "db-password",
  "bytes": "aHVudGVyMg==",
  "version": "v3",
  "sensitive": true,
  "not_after": "2026-08-01T00:00:00Z",
  "metadata": {},
  "kind": "",
  "resolved_at": "2026-07-26T21:04:11Z",
  "stale": false
}
```

- **`bytes`** is base64-encoded (`encoding/json`'s standard `[]byte` handling).
- **`version`**, **`sensitive`**, **`not_after`**, **`bytes`**, **`kind`**, **`resolved_at`**, and **`stale`** are all `omitempty`; `metadata` is never omitted (a `nil` map becomes `{}`, never `null`).
- **`resolved_at`** is when the replica answering you last fetched these bytes from upstream, and **`stale`** says it is serving them as last-known-good while upstream currently fails. They exist for [multi-replica deployments](/docs/server/ha/), where each replica watches on its own schedule: comparing `resolved_at` is how a client notices it reached a replica that is behind one it already spoke to. `resolved_at` dates the value, not the last attempt, so a failed refresh carries it forward unchanged.

A whole-request failure (auth failure, a malformed batch body, `GET /v1/values/{name}` for an unbound or unresolvable name) is:

```json
{"error": {"kind": "not_found", "message": "binding not found"}}
```

A batch or watch response never fails the whole request over one bad name: a single name that is denied, unbound, or unresolvable becomes exactly that one entry, shaped like an error but carrying the requested `name`:

```json
{"name": "some-other-binding", "error": {"kind": "permission_denied", "message": "permission denied"}}
```

**Distinguish success from failure by the presence of the `error` field, never by whether `bytes` is present.** A genuinely empty (zero-length) secret has its `bytes` key omitted exactly the way an error entry does, so "no bytes means error" misreads a legitimate empty value.

`kind` round-trips through core's `mamori.ErrorKind`/`mamori.SentinelFor`, so a client can recover the original sentinel: `mamori.SentinelFor(mamori.Kind("permission_denied"))` gives back `mamori.ErrPermissionDenied`. The kind-to-status mapping:

| `kind` | HTTP status |
| --- | --- |
| `not_found` | 404 |
| `invalid` | 400 |
| `unauthenticated` | 401 |
| `permission_denied` | 403 |
| `rate_limited` | 429 |
| `unavailable` | 503 |
| `unknown` | 500 |

## Read `kind` on a success: fresh vs. stale-but-serving

`kind` appears in two structurally different places, and conflating them is the easiest client mistake:

- **On an error entry** (`{"error":{"kind":...}}`), `kind` is why the request failed.
- **On a successful value** (`bytes` present, no `error`), a non-empty `kind` means the server is serving a last-known-good value while the binding's upstream is currently failing. This is an annotation on a success, not a failure.

A client that ignores `kind` will silently keep using a value the server knows is stale; a client that treats any non-empty `kind` as an error will reject usable data. Check `error` presence for success vs. failure, and use `kind` on a success only to decide whether to log or alert on staleness.

## Resolve one binding with `GET /v1/values/{name}`

Fetch a single binding by name, URL-path-escaped:

```text
GET /v1/values/db-password
```

`{name}` is a binding name the operator declared on the server, never an upstream ref (a client cannot ask the server to resolve an arbitrary ref). On success the body is the single [value shape](#response-shapes) above with a `200`. An unbound name, or one whose upstream has never resolved, returns the `{"error":{...}}` envelope with the matching status from the kind table (for example `404` with `kind: not_found` for a name the server does not serve). A last-known-good value being served while its upstream is currently failing still comes back as a `200` value with a non-empty `kind`, not as an error (see [fresh vs. stale-but-serving](#read-kind-on-a-success-fresh-vs-stale-but-serving)).

## Resolve a batch with `POST /v1/values`

Resolve several bindings in one round trip. The body lists the names to resolve:

```json
{"names": ["db-password", "api-key", "region"]}
```

The response is a `values` array with one entry per requested name, in request order:

```json
{
  "values": [
    {"name": "db-password", "bytes": "aHVudGVyMg==", "sensitive": true, "metadata": {}},
    {"name": "api-key", "error": {"kind": "permission_denied", "message": "permission denied"}},
    {"name": "region", "bytes": "dXMtZWFzdC0x", "metadata": {}}
  ]
}
```

Each entry is independently a value or a per-name error (same [shapes](#response-shapes) as the single route), and each name is authorized on its own. One denied, unbound, or unresolvable name becomes that single entry's error; the request itself still returns `200`. This lets a client resolve every field of a config struct in a single request rather than one call per field, which is exactly what the [`mamori://` client](/docs/providers/mamori/) does through its `BatchProvider`. A body that is not `{"names":[...]}` is a whole-request `400` (`kind: invalid`).

## Subscribe to changes with `GET /v1/watch`

```text
GET /v1/watch?name=db-password&name=api-key
```

`/v1/watch` opens a Server-Sent Events stream: an `error` frame at subscribe time for any denied or unbound name (the stream still opens and stays live for the names that were allowed), then an `update` frame each time an allowed name's resolved state changes, plus a periodic heartbeat comment so idle-closing proxies do not drop the connection. This is a fast poll (roughly 200ms) of already-resolved local state, not push from the backend. The stream ends when the client disconnects or a write fails; reconnecting is the client's responsibility.

## Probe liveness with `GET /v1/healthz`

```json
{"status": "ok"}
```

A fixed, unconditional `200`, never any binding detail. It is exempt from authentication and authorization, so a probe with no credential still works, and it can never become a way to learn what the server holds.

## Probe readiness with `GET /v1/readyz`

```json
{"status": "ready"}
```

Liveness says the process is alive; readiness says whether it should be sent traffic. Route your load balancer on this one. It answers `200` with `ready`, or `503` with `priming` (bindings still resolving, so the cache is cold) or `draining` (shutting down). Like `healthz` it is unauthenticated and therefore reports a bare status with no binding names, counts, or node identity.

See [High availability](/docs/server/ha/) for how to use it when running several replicas.

## Next

- [Server auth and policy](/docs/server/authorization/) - the authenticate-then-authorize gates these routes enforce.
- [High availability](/docs/server/ha/) - `readyz`, draining, and the freshness fields across replicas.
- [Providers: mamori](/docs/providers/mamori/) - the `mamori://` client that speaks this protocol.
- [Config server overview](/docs/server/) - the fan-out model and blast radius.
