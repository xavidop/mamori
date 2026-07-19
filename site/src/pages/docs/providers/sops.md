---
layout: ../../../layouts/DocsLayout.astro
title: SOPS provider
---

# SOPS

Decrypts a [SOPS](https://github.com/getsops/sops)-encrypted file and **watches it with fsnotify**, so a re-encrypted file is hot-reloaded.

| | |
| --- | --- |
| Scheme | `sops://` |
| Module | `github.com/xavidop/mamori/providers/sops` |
| Sensitive | yes |
| Watch | fsnotify (native) |
| Auth | `SOPS_AGE_KEY` / `SOPS_AGE_KEY_FILE`, or KMS credentials |

## Install

```bash
go get github.com/xavidop/mamori/providers/sops
```

```go
import _ "github.com/xavidop/mamori/providers/sops"
```

## Using the ref

A `sops://` ref points at a SOPS-encrypted file on disk, optionally selecting one key inside the decrypted document.

```text
sops://<path/to/file.enc.yaml>[#key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<path/to/file.enc.yaml>` | yes | Path to the encrypted file. Relative (`sops://secrets/app.enc.json`) or absolute with a leading slash (`sops:///etc/secrets/db.enc.yaml`). The format (`yaml`, `json`, `dotenv`, `binary`) is inferred from the extension. |
| `#key` | no | Treat the decrypted document as a JSON/YAML object and return a single field. Without it, the whole decrypted content is the value. |

**Examples**

- `sops:///etc/secrets/db.enc.yaml#password` - decrypts the absolute-path YAML file and returns just its `password` field.
- `sops://secrets/app.enc.json#api_key` - decrypts the relative-path JSON file and returns the `api_key` field.
- `sops://config/app.enc.yaml` - returns the whole decrypted document when you want the entire file, not one key.

```go
type Config struct {
	DBPassword secret.String `source:"sops:///etc/secrets/db.enc.yaml#password"` // absolute path
	APIKey     secret.String `source:"sops://secrets/app.enc.json#api_key"`      // relative path
}
```

Values are always marked `Sensitive`. `Value.Version` is a hash of the encrypted file's size and modification time, so re-resolving an unchanged file is cheap and never decrypts twice just to compare.

## Auth and watch

mamori does not manage keys; it calls SOPS to decrypt, so whatever key material SOPS finds in the environment applies (`SOPS_AGE_KEY` for age, or the configured AWS/GCP/Azure KMS credentials). The provider watches the encrypted file with fsnotify (watching the parent directory to catch atomic renames) and re-decrypts on change.

## Explicit configuration

The decryption step is injectable, which is how the conformance kit runs without real keys:

```go
import sopsprov "github.com/xavidop/mamori/providers/sops"

mamori.WithProvider(sopsprov.New(
	sopsprov.WithDecrypt(func(path, format string) ([]byte, error) { /* ... */ }),
))
```

Verified by unit tests (yaml/json key selection, format detection, not-found, fsnotify watch) with an injected decrypt function. Real SOPS decryption with a generated age key is covered by `//go:build integration` tests.
