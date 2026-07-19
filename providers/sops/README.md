# mamori SOPS provider

[SOPS](https://github.com/getsops/sops) provider for [mamori](https://github.com/xavidop/mamori). Decrypts a SOPS-encrypted file and, like the built-in `file://` provider, **watches it with fsnotify** so re-encrypted files are hot-reloaded.

```bash
go get github.com/xavidop/mamori/providers/sops
```

```go
import _ "github.com/xavidop/mamori/providers/sops" // registers sops://
```

## Scheme

```
sops://<path/to/file.enc.yaml>[#key]
```

```go
type Config struct {
    DBPassword secret.String `source:"sops:///etc/secrets/db.enc.yaml#password"` // absolute path
    APIKey     secret.String `source:"sops://secrets/app.enc.json#api_key"`      // relative path
}
```

- Format (`yaml`, `json`, `dotenv`, `binary`) is inferred from the file extension.
- With `#key`, the decrypted document is treated as a JSON/YAML object and the key is selected (via `mamori.SelectKey`). Without `#key`, the whole decrypted content is the value.
- Values are marked `Sensitive`. `Value.Version` is a hash of the file size + mtime.

## Authentication

Whatever key material SOPS itself uses from the ambient environment: `SOPS_AGE_KEY` / `SOPS_AGE_KEY_FILE` for age, or the configured AWS/GCP/Azure KMS credentials. mamori does not manage keys; it calls SOPS to decrypt.

## Watch

Native via fsnotify on the encrypted file (watches the parent directory to catch atomic renames), re-decrypting on change.

## What is verified

- ✅ Unit tests and the [`providertest`](../../providertest) conformance kit run with an **injected decrypt function** (`WithDecrypt`), so ref/key selection, format detection, not-found handling, and the fsnotify watch are all exercised without needing real SOPS keys.
- ⚠️ Real SOPS decryption (age/KMS) is exercised by `//go:build integration` tests that generate a key and encrypt a fixture, **not** run in CI by default.

Passes the mamori conformance kit. 🛡️
