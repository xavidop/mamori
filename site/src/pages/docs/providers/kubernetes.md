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

Verified against `client-go`'s fake clientset, which supports watch - so the watch conformance checks run for real, not skipped. Live-cluster behavior is covered by `//go:build integration` tests.
