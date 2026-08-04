---
layout: ../../../layouts/DocsLayout.astro
title: Bitwarden Secrets Manager
---

# Bitwarden Secrets Manager

Load a secret from [Bitwarden Secrets Manager](https://bitwarden.com/products/secrets-manager/), the machine-account product behind the `bws` CLI. Pure standard library on top of [`httpcore`](/docs/providers/https) - no Bitwarden SDK, no cgo, no `golang.org/x/crypto`.

This is a provider for **Secrets Manager**, not for the consumer password manager: the two speak different APIs and use different key hierarchies.

| | |
| --- | --- |
| Scheme | `bitwarden-sm://` |
| Module | `github.com/xavidop/mamori/providers/bitwarden` |
| Sensitive | yes (always) |
| Watch | poll |
| Auth | `BWS_ACCESS_TOKEN` |

## Install

```bash
go get github.com/xavidop/mamori/providers/bitwarden
```

```go
import _ "github.com/xavidop/mamori/providers/bitwarden" // registers bitwarden-sm://
```

## Scheme

```
bitwarden-sm://<secret-uuid>[#key]
```

```go
type Config struct {
    StripeKey secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d"`
    DBPass    secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#password"`
}
```

- The path is the secret's **UUID** - the Bitwarden UI's "Copy secret ID", and what `bws secret get` takes.
- The value resolved is the secret's decrypted `value`. A `#key` fragment goes to `SelectKey`, selecting out of that value when it is a JSON document, exactly as in every other provider.
- Values are always marked `Sensitive`. `Value.Version` is the secret's `revisionDate`, a plaintext field returned beside the ciphertext, so change detection never touches the decrypted value.

### Why a UUID and not a name

Bitwarden's list endpoint returns each secret's **name as ciphertext** and omits its value entirely, so resolving by name would mean fetching and decrypting every secret in the organization on every poll - and a name is not unique across projects anyway. A UUID is stable, unique, and costs one request.

A ref that is not a UUID is rejected with `invalid` before any request is sent. That is deliberately not `not_found`, because `not_found` is the one kind that makes mamori apply a field's **default**: a malformed ref classified that way would quietly become a default value instead of an error. Refs carry `${VAR}` interpolation, so an unset variable really does arrive as an empty or partial path.

## Authentication

A machine account access token, via `BWS_ACCESS_TOKEN` (the same variable `bws` reads) or explicitly:

```go
mamori.WithProvider(bitwarden.New(bitwarden.WithAccessToken("0.uuid.secret:key==")))
```

Self-hosted, and Bitwarden's EU cloud, deriving both endpoints the way `bws` does (`base + "/identity"` and `base + "/api"`):

```go
mamori.WithProvider(bitwarden.New(bitwarden.WithServerURL("https://vault.bitwarden.eu")))
mamori.WithProvider(bitwarden.New(bitwarden.WithServerURL("https://vault.example.com")))
```

The token is read **lazily, at resolve time**, so a blank import is safe even when no token is set at process start. It is re-read on every refresh, so a rotated `BWS_ACCESS_TOKEN` is picked up without a restart.

Both endpoints must be `https://`. The exchange POSTs the client secret in its form body and the API returns the organization's secrets, so cleartext exposes both. `WithAllowInsecure(true)` opts into `http://` for a local install and permits nothing else, not even a different scheme.

## How decryption works

Bitwarden is end to end encrypted: the API returns ciphertext and the server cannot read it. A provider that returned what the API returned would be returning base64, not a secret. So this provider performs the full client-side unwrap.

| Step | What happens | Standard library used |
| --- | --- | --- |
| 1 | Split the access token `0.<uuid>.<secret>:<base64 16 bytes>` | `strings`, `encoding/base64` |
| 2 | Stretch its 16-byte key to 64 bytes: HKDF-Extract with salt `bitwarden-accesstoken`, then HKDF-Expand with info `sm-access-token` | `crypto/hkdf`, `crypto/sha256` |
| 3 | `POST /connect/token` (`grant_type=client_credentials`, `scope=api.secrets`), read `encrypted_payload` | `net/url`, `httpcore` |
| 4 | Decrypt that payload with the step 2 key; it yields `{"encryptionKey":"..."}`, the organization key | `crypto/aes`, `crypto/cipher`, `crypto/hmac` |
| 5 | `GET /secrets/<uuid>`, decrypt its `value` with the organization key | same as step 4 |

Steps 4 and 5 are the same primitive: a **type 2 EncString**, `2.<iv>|<ciphertext>|<mac>`, each part standard base64. AES-256-CBC with HMAC-SHA256 in Encrypt-then-MAC, a 16-byte IV, a 32-byte tag, PKCS#7 padding, and a 64-byte key split into a 32-byte AES sub-key followed by a 32-byte MAC sub-key. The MAC covers `iv || ciphertext`.

`crypto/hkdf` is what makes this possible at all. It entered the Go standard library in **Go 1.24**; before that, HKDF meant `golang.org/x/crypto`, which mamori does not depend on, and Bitwarden's own SDK is a Rust core reached through cgo. This module requires Go 1.26.

### Security properties

- **The MAC is verified before decryption**, using `hmac.Equal` and never `bytes.Equal`. Verifying first means a forged ciphertext never reaches the cipher, removing the padding-oracle class of attack outright. `bytes.Equal` returns as soon as two bytes differ, so its timing reveals how many leading bytes of a candidate tag were right.
- **EncString type 0 is refused**, not decrypted: it is AES-CBC with no MAC, and Bitwarden's own source marks it as a variant that must never be used to encrypt.
- **EncString type 7 is refused with a reason.** It is a COSE Encrypt0 message carrying XChaCha20-Poly1305; neither that cipher nor CBOR is in the Go standard library, so this provider cannot decrypt it and reports `unavailable` naming the missing primitive. **This is the main forward risk**: type 7 is Bitwarden's preferred variant for new data, and if Secrets Manager values migrate to it this provider will fail loudly rather than silently.
- **No key material sits in a readable struct field.** Every derived key, the organization key, the client secret, and the bearer token live inside closures, because `fmt`'s `%v`, `%+v` and `%#v` walk unexported fields by reflection and reflection cannot call a `String` method on a value it reaches that way. A redacting wrapper type would not have helped.
- **Intermediates are zeroed**: HKDF outputs, the decrypted identity payload, the raw organization key bytes, and the full CBC buffer including padding.
- **No key, token, ciphertext, or plaintext ever reaches an error message.**

## Watch

Secrets Manager exposes no push channel, so mamori polls (interval + jitter). Configure with `mamori.WithPollInterval`. The access token is cached and refreshed 30 seconds before expiry by default (`WithLeeway`), and concurrent resolves starting cold share a single exchange rather than one each.

## Error classification

Both endpoints classify through `httpcore.ClassifyStatus`:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

A rejected token exchange also carries RFC 6749's `error` code, such as `invalid_client` - a fixed token, never free text and never derived from the client secret. `error_description` is deliberately not read, and the secrets endpoint surfaces no body at all, since its bodies carry secret material.

Errors name the machine account UUID on purpose: it is an identifier, not a credential, and without it a deployment with several machine accounts cannot tell which one failed.

## What is verified

- ✅ **Vendor-verified key derivation**: two test vectors copied verbatim from Bitwarden's `shareable_key.rs` match byte for byte.
- ✅ **Vendor-verified cipher, padding, MAC order, and EncString format**: Bitwarden's published ciphertext, published key, and published plaintext from `aes256_cbc_hmac_sha256_ae.rs`. A self-consistent round trip could not establish this - it would pass just as green with the MAC computed over the ciphertext alone, or the key halves swapped.
- ⚠️ **Self-consistent only**: the end-to-end wiring. The in-process fake encrypts with its own implementation of the same scheme, so the conformance tests prove the plumbing round trips, not that the format is Bitwarden's. The vendor vectors carry that claim.
- ❌ **Not verified against a live organization at authoring time**: no ciphertext produced by Bitwarden's own servers has been decrypted by this code. The integration test exists for that gap and skips cleanly without credentials:

  ```bash
  export BWS_ACCESS_TOKEN=0.uuid.secret:key==
  export MAMORI_BWS_SECRET_ID=6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d
  GOWORK=off go test -tags integration ./...
  ```

- ✅ Conformance through the `providertest` kit against an in-process `http.RoundTripper`, a separate status-to-kind table test, `-race`, and mutation checks on the MAC verification, the HKDF constants, and the reflection-leak guarantees.
