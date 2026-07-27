# mamori Kubernetes provider

Kubernetes Secret and ConfigMap providers for [mamori](https://github.com/xavidop/mamori), built on `client-go`. Values are **natively watched** via the Kubernetes watch API - the same mechanism informers use - so changes propagate without polling.

```bash
go get github.com/xavidop/mamori/providers/k8s
```

```go
import _ "github.com/xavidop/mamori/providers/k8s" // registers k8s-secret:// and k8s-cm://
```

## Schemes

| Scheme | Backend | Sensitive | Watch |
|---|---|---|---|
| `k8s-secret://<namespace>/<name>[#key]` | core/v1 Secret | ✅ | **native** |
| `k8s-cm://<namespace>/<name>[#key]` | core/v1 ConfigMap | ❌ | **native** |

```go
type Config struct {
    DBPassword secret.String `source:"k8s-secret://prod/db-creds#password"`
    CACert     []byte        `source:"k8s-secret://prod/tls#ca.crt"`
    LogLevel   string        `source:"k8s-cm://prod/app-config#log_level"`
}
```

- With `#key`, the value is the corresponding `data` entry (client-go base64-decodes Secret data for you; ConfigMap `binaryData` is also consulted).
- Without `#key`, the whole data map is JSON-encoded as an object of string values.
- `Value.Version` is the object's `ResourceVersion` - monotonic, native change detection.

`ca.crt`, `tls.crt`, and `tls.key` are literal key names, not paths. A mamori fragment is only a JSON Pointer when it begins with `/`, so a dotted key addresses exactly the key it names. A Kubernetes Secret's `data` is a flat map with no nesting to point into, so this provider only ever does a literal lookup.

## Authentication

In-cluster config when running in a Pod; otherwise the default kubeconfig resolution (`KUBECONFIG`, then `~/.kube/config`). Override explicitly:

```go
mamori.WithProvider(k8s.New(k8s.WithKubeconfig("/path/to/kubeconfig")))
mamori.WithProvider(k8s.NewConfigMap(k8s.WithClient(myClientset)))
```

## Watch

`Watch` opens a name-scoped watch and emits an `Update` on every Added/Modified event; it re-establishes (re-list + re-watch) if the server-side watch ends while the context is alive, and closes cleanly on cancellation.

## Error classification

| Kubernetes condition | mamori kind |
|---|---|
| `IsNotFound` | `not_found` |
| `IsForbidden` (RBAC) | `permission_denied` |
| `IsUnauthorized` | `unauthenticated` |
| `IsTooManyRequests` | `rate_limited` |
| `IsServiceUnavailable`, `IsTimeout`, `IsServerTimeout` | `unavailable` |
| `IsBadRequest`, `IsInvalid` | `invalid` |
| Malformed ref (not `<namespace>/<name>`) | `invalid` |
| anything else | `unknown` |

Detection uses the `apierrors` predicates rather than raw status codes, so it
stays correct across API versions. The underlying `*StatusError` remains
reachable, so `apierrors.IsForbidden` still works on an error that has passed
through mamori.

## What is verified

- ✅ Unit tests and the full [`providertest`](../../providertest) conformance kit run against `client-go`'s fake clientset - which supports **watch**, so the watch-emits-on-mutate and watch-closes-on-cancel conformance checks run for real (not skipped).
- ⚠️ Live-cluster behavior is exercised by `//go:build integration` tests (envtest / a real cluster), **not** run in CI by default.

Passes the mamori conformance kit, including native watch. 🛡️
