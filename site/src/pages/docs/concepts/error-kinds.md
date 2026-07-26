---
layout: ../../../layouts/DocsLayout.astro
title: Error kinds
---

# Error kinds

Every resolve failure carries a coarse, provider-independent classification you read with `mamori.ErrorKind(err)`. It exists so diagnostics can tell a misconfiguration from an outage.

## Kind values

The typed `Kind` values, each paired with the sentinel `errors.Is` matches:

| `Kind` | Wire value | Sentinel (`errors.Is`) | Meaning |
| --- | --- | --- | --- |
| `KindNotFound` | `not_found` | `ErrNotFound` | Missing key, secret, path, or version. The only kind that drives resolution behavior. |
| `KindPermissionDenied` | `permission_denied` | `ErrPermissionDenied` | Authenticated but not authorized (IAM deny, Vault policy, Kubernetes RBAC). |
| `KindUnauthenticated` | `unauthenticated` | `ErrUnauthenticated` | Credentials missing, malformed, or expired; identity not proven. |
| `KindUnavailable` | `unavailable` | `ErrUnavailable` | Backend unreachable or unresponsive (network, DNS, timeout, 5xx, open breaker). |
| `KindRateLimited` | `rate_limited` | `ErrRateLimited` | Throttled or quota-exhausted by the backend. |
| `KindInvalid` | `invalid` | `ErrInvalid` | Ref malformed for the provider, or payload could not be parsed. |
| `KindUnknown` | `unknown` | (none) | An error the provider could not map. A legal outcome, not a failure. |

Only `not_found` changes behavior by default, since it is what triggers a field's `default:` or `optional` handling. The rest are diagnostic. The one opt-in exception is `onfail:"default"` on a source chain, which explicitly treats a non-`not_found` error like absence too (see [Source chains and precedence](/docs/concepts/source-chains/#the-onfail-policy)).

## Reaching the sentinels

`errors.Is` reaches the sentinels through the error chain, so you can branch on a specific condition:

```go
if errors.Is(err, mamori.ErrPermissionDenied) {
    // the credential is fine, the authorization is not
}
```

`ErrorKind(nil)` returns the empty `Kind`; an error carrying no recognizable sentinel returns `KindUnknown`. A `context.DeadlineExceeded` is reported as `KindUnavailable` (the backend did not respond in time), while a `context.Canceled` stays `KindUnknown` (the caller withdrew the request, which says nothing about the backend). `KindUnknown` and the empty `Kind` have no sentinel of their own.

`SentinelFor(k)` is the inverse: it turns a `Kind` back into its sentinel error, or `nil` for `KindUnknown` and the empty `Kind`. A `nil` return means "no sentinel corresponds to this kind," never "the operation succeeded."

## See also

- [Concepts overview](/docs/concepts/) - refs, providers, and the reconciler.
- [Source chains and precedence](/docs/concepts/source-chains/) - how `not_found` versus other errors drive a chain.
- [Observability](/docs/observability/) - `Kind` values on `Report` / `FieldStatus`.
