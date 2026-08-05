---
layout: ../../../layouts/DocsLayout.astro
title: Kubernetes provider
---

# Kubernetes

Secrets and ConfigMaps, with **native watch** via the Kubernetes watch API (the same mechanism informers use), built on `client-go`.

| | |
| --- | --- |
| Schemes | `k8s-secret://` `k8s-cm://` |
| Module | `github.com/xavidop/mamori/providers/k8s` |
| Sensitive | Secret: yes · ConfigMap: no |
| Watch | native |
| Auth | in-cluster config, else `KUBECONFIG` / `~/.kube/config` |

## Install

```bash
go get github.com/xavidop/mamori/providers/k8s
```

```go
import _ "github.com/xavidop/mamori/providers/k8s" // registers k8s-secret:// and k8s-cm://
```

## Using the ref

A `k8s-secret://` or `k8s-cm://` ref points at one Secret or ConfigMap in a namespace, optionally selecting one entry from its data map.

```text
k8s-secret://<namespace>/<name>[#key]
k8s-cm://<namespace>/<name>[#key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<namespace>` | yes | The namespace that holds the object. |
| `<name>` | yes | The Secret or ConfigMap name. |
| `#key` | no | Return one entry of the object's `data` map. Without it, the whole data map is JSON-encoded as an object of string values. |

**Examples**

- `k8s-secret://prod/db-creds#password` - returns the `password` entry of the `db-creds` Secret in namespace `prod` (client-go base64-decodes it for you).
- `k8s-secret://prod/tls#ca.crt` - returns the raw `ca.crt` bytes from the `tls` Secret.
- `k8s-cm://prod/app-config#log_level` - returns the `log_level` entry of the `app-config` ConfigMap.
- `k8s-cm://prod/app-config` - returns the whole ConfigMap data map as a JSON object.

`ca.crt`, `tls.crt`, and `tls.key` are literal key names, not paths. A mamori fragment is only a JSON Pointer when it begins with `/`, so a dotted key addresses exactly the key it names. A Kubernetes Secret's `data` is a flat map with no nesting to point into, so this provider only ever does a literal lookup.

```go
type Config struct {
	DBPassword secret.String `source:"k8s-secret://prod/db-creds#password"`
	CACert     []byte        `source:"k8s-secret://prod/tls#ca.crt"`
	LogLevel   string        `source:"k8s-cm://prod/app-config#log_level"`
}
```

For a `#key` on a ConfigMap, `data` is consulted first and then `binaryData`. `Value.Version` is the object's `ResourceVersion`, giving monotonic, native change detection. Secret values are marked `Sensitive`; ConfigMap values are not.

## Watch

`Watch` opens a name-scoped watch and emits an `Update` on every Added/Modified event. If the server-side watch ends while the context is alive it re-establishes (re-list + re-watch), and it closes cleanly on cancellation. This is a genuine push: no polling.

## Explicit configuration

```go
import k8sprov "github.com/xavidop/mamori/providers/k8s"

mamori.WithProvider(k8sprov.New(k8sprov.WithKubeconfig("/home/me/.kube/config")))
mamori.WithProvider(k8sprov.NewConfigMap(k8sprov.WithClient(myClientset)))
```

`Close()` is idempotent and terminal: after it returns, every `Resolve` and `Watch` report `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting the cluster. It releases the idle HTTP connections behind a clientset this provider built itself, whether through the default kubeconfig/in-cluster resolution or through `WithClientFactory`, but only when releasing them is safe: a clientset configured with no TLS material at all resolves, several layers down, to the shared `http.DefaultTransport`, and `Close` detects and skips that narrow case rather than evict connections belonging to unrelated code. A clientset injected directly with `WithClient` is never touched.

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

Detection uses the `apierrors` predicates rather than raw status codes, so it stays correct across API versions. The underlying `*StatusError` remains reachable, so `apierrors.IsForbidden` still works on an error that has passed through mamori.

Verified against `client-go`'s fake clientset, which supports watch - so the watch conformance checks run for real, not skipped. Live-cluster behavior is covered by `//go:build integration` tests.
