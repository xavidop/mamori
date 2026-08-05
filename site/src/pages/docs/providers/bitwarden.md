---
layout: ../../../layouts/DocsLayout.astro
title: Bitwarden Secrets Manager
---

# Bitwarden Secrets Manager

Load a secret from [Bitwarden Secrets Manager](https://bitwarden.com/products/secrets-manager/), the machine-account product behind the `bws` CLI, in pure standard library on top of [`httpcore`](/docs/providers/https). This is a provider for **Secrets Manager** and not for the consumer password manager: the two speak different APIs and use different key hierarchies.

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

This module requires **Go 1.26**: it decrypts values with `crypto/hkdf`, which only entered the standard library in Go 1.24.

## Using the ref

A `bitwarden-sm://` ref points at one secret by id, optionally selecting a field from a JSON value stored in it.

```text
bitwarden-sm://<secret-uuid>
bitwarden-sm://<secret-uuid>[#field-or-pointer]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<secret-uuid>` | yes | The secret's **UUID**: the Bitwarden UI's "Copy secret ID", and what `bws secret get` takes. |
| `#field` / `#/json/pointer` | no | Select a field from the decrypted value via `mamori.SelectKey`: a literal top-level key, or an RFC 6901 JSON Pointer for a nested field. |

**Examples**

- `bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d` returns the whole decrypted value.
- `bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#password` returns the `password` field of a JSON secret.
- `bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#/db/password` selects a nested field by JSON Pointer.

```go
type Config struct {
    StripeKey secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d"`
    DBPass    secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#password"`
}
```

**A ref takes a UUID, never a secret's name.** Bitwarden's list endpoint returns each name as ciphertext and omits the value entirely, so resolving by name would mean fetching and decrypting every secret in the organization on every poll, and a name is not unique across projects anyway.

**A ref that is not a UUID is `invalid`, not `not_found`.** It is rejected before any request is sent. `not_found` is the kind that makes mamori apply a field's **default**, so a malformed ref, from an unset `${VAR}` in an interpolated one for instance, fails loudly instead of quietly becoming a default value.

Values are always marked `Sensitive`. `Value.Version` is the secret's `revisionDate`, a plaintext field returned beside the ciphertext, so change detection never touches the decrypted value.

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

Both endpoints must be `https://`. The exchange posts the client secret in its form body and the API returns the organization's secrets, so cleartext exposes both. `WithAllowInsecure(true)` opts into `http://` for a local install and permits nothing else, not even a different scheme.

## Encrypted values

Bitwarden is end to end encrypted: the API returns ciphertext and the server cannot read it, so this provider performs the client-side unwrap itself. Your access token is stretched into a key that decrypts the organization key out of the token exchange, and the organization key decrypts the secret. None of it is configurable, but two limits follow from it.

**A value encrypted as EncString type 7 cannot be read.** That variant is a COSE Encrypt0 message carrying XChaCha20-Poly1305, and neither that cipher nor CBOR is in the Go standard library, so such a value reports `unavailable` naming the missing primitive rather than failing quietly. Type 7 is Bitwarden's preferred variant for new data, so this is the limitation most likely to matter later.

**EncString type 0 is refused rather than decrypted.** It is AES-CBC with no MAC, and Bitwarden's own source marks it as a variant that must never be used to encrypt.

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

A rejected token exchange also carries RFC 6749's `error` code, such as `invalid_client`: a fixed token, never free text and never derived from the client secret. The secrets endpoint surfaces no body at all, since its bodies carry secret material. No key, token, ciphertext, or plaintext reaches an error message, and none of them sits in a readable struct field, so none can surface in a `%v` or `%+v` dump either.

Errors name the machine account UUID on purpose. It is an identifier, not a credential, and without it a deployment with several machine accounts cannot tell which one failed.

## Watch

Secrets Manager exposes no push channel, so mamori polls (interval + jitter). Configure with `mamori.WithPollInterval`. The access token is cached and refreshed 30 seconds before expiry by default (`WithLeeway`), and concurrent resolves starting cold share a single exchange rather than one each.

## Configuration

| Option | Effect |
| --- | --- |
| `WithAccessToken(token)` | Machine account access token; empty falls back to `BWS_ACCESS_TOKEN` |
| `WithServerURL(base)` | A self-hosted install or the EU cloud, deriving both endpoints from one base |
| `WithIdentityURL(u)`, `WithAPIURL(u)` | Override one endpoint alone |
| `WithHTTPClient(c)` | Inject a custom `*http.Client` for both endpoints |
| `WithMaxBody(n)` | Cap the response size accepted from either endpoint |
| `WithLeeway(d)` | How far before its stated expiry a cached token is renewed |
| `WithAllowInsecure(yes)` | Permit an `http://` endpoint, and nothing else |

`Close()` is idempotent and terminal: after it returns, every `Resolve` reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting Bitwarden. It also returns the HTTP client's idle connections to the pool, but only when that client's `Transport` is non-nil. `New`'s own default client (unless overridden with `WithHTTPClient`) leaves `Transport` unset, and Go resolves a nil `Transport` to the shared `http.DefaultTransport`; releasing idle connections there would evict connections belonging to unrelated code in the same process, so `Close` skips it. A client injected with `WithHTTPClient` is never closed or invalidated either way, only its idle connections may be released.

The key derivation and the cipher are checked against test vectors published by Bitwarden, and the rest against an in-process HTTP fake, so the conformance suite runs without a Bitwarden organization. No ciphertext produced by Bitwarden's own servers has been decrypted by this code at authoring time; a `//go:build integration` test closes that gap against a real organization when `BWS_ACCESS_TOKEN` and `MAMORI_BWS_SECRET_ID` are set, and skips cleanly when they are not.
