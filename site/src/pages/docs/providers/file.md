---
layout: ../../../layouts/DocsLayout.astro
title: file provider
---

# file

Reads a file's contents and **natively watches it with fsnotify**, so file-backed values are hot-reloaded. Built into the core module and registered automatically.

| | |
| --- | --- |
| Scheme | `file://` |
| Module | core (built-in) |
| Sensitive | no |
| Watch | fsnotify (native) |
| Auth | filesystem permissions |

## Using the ref

A `file://` ref points at one file on disk; the file's contents become the value.

```text
file:///absolute/path
file://relative/path
```

| Part | Required | What it means |
| --- | --- | --- |
| `file://` | yes | Scheme. A third slash (`file:///`) starts an absolute path; `file://name` is relative to the working directory. |
| `/absolute/path` | yes | The file to read. Its full contents (raw bytes) are the value. |
| `?debounce=` | no | Coalescing window for watch events. Set `?debounce=0` to emit the instant the file changes (see Watch). |

**Examples**

- `file:///etc/tls/tls.crt` reads a certificate into a raw `[]byte` field.
- `file:///etc/tls/tls.key` into a `secret.Bytes` field keeps the private key redacted in logs.
- `file:///etc/app/config.yaml` with `flatten:"yaml"` decodes the file into a nested struct.
- `file:///etc/tls/tls.crt?debounce=0` hot-reloads the moment the cert is atomically replaced.

```go
type Config struct {
	TLSCert []byte        `source:"file:///etc/tls/tls.crt"`
	TLSKey  secret.Bytes  `source:"file:///etc/tls/tls.key"`
	Config  AppConfig     `source:"file:///etc/app/config.yaml" flatten:"yaml"`
}

cfg, err := mamori.Load[Config](ctx) // file is already registered
```

## Watch

`file://` watches the target's **parent directory** with fsnotify, so it catches atomic replaces (write-to-temp then rename) - the pattern used by Kubernetes secret mounts and cert-renewal tools. It re-reads and emits on write, create, or rename of the target, and closes cleanly on context cancellation.

For a certificate that should update the instant it changes, disable coalescing on that field:

```go
Cert []byte `source:"file:///etc/tls/tls.crt?debounce=0"`
```

## Notes

- `Value.Version` is a hash of the file's size and modification time, so unchanged files are not re-read.
- A missing file resolves to not-found, so `default:` / `optional:"true"` applies.
- `file://` values are non-sensitive by default; use `secret.Bytes` for private keys so they stay redacted.
