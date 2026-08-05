# mamori - Bitwarden Secrets Manager provider

[Bitwarden Secrets Manager](https://bitwarden.com/products/secrets-manager/)
provider for [mamori](https://github.com/xavidop/mamori). Pure standard
library on top of [`httpcore`](../httpcore) - no Bitwarden SDK, no cgo, no
`golang.org/x/crypto`.

This is a provider for **Secrets Manager**, the machine-account product behind
the `bws` CLI. It is not a provider for the consumer password manager, which
speaks a different API and a different key hierarchy.

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

- The path is the secret's **UUID**, which is what the Bitwarden UI's "Copy
  secret ID" yields and what `bws secret get` takes.
- The resolved value is the secret's decrypted `value`. A `#key` fragment goes
  to `mamori.SelectKey`, so it selects out of that value when it is a JSON
  document, exactly as in every other provider.
- Values are always `Sensitive`. `Value.Version` is the secret's
  `revisionDate`, a plaintext field the API returns beside the ciphertext, so
  change detection never touches the decrypted value.

### Why a UUID and not a name

Bitwarden's list endpoint (`GET /organizations/{orgId}/secrets`) returns each
secret's **name as ciphertext** and omits its value entirely. Resolving a name
would therefore mean fetching and decrypting every secret in the organization
on every poll, and a name is not unique across projects anyway. A UUID is
stable, unique, and costs one request. A ref that is not a UUID is rejected
with `mamori.ErrInvalid` before any request is sent.

That error is deliberately not `ErrNotFound`: `ErrNotFound` is the one kind
that makes mamori apply a field's **default**, so a malformed ref classified
that way would quietly become a default value instead of an error. Refs carry
`${VAR}` interpolation, so an unset variable really does arrive here as an
empty or partial path.

## Authentication

A machine account access token, via `BWS_ACCESS_TOKEN` (the same variable
`bws` reads) or explicitly:

```go
mamori.WithProvider(bitwarden.New(bitwarden.WithAccessToken("0.uuid.secret:key==")))
```

Self-hosted and Bitwarden's EU cloud, which derive both endpoints the way
`bws` does (`base + "/identity"` and `base + "/api"`):

```go
mamori.WithProvider(bitwarden.New(bitwarden.WithServerURL("https://vault.bitwarden.eu")))
mamori.WithProvider(bitwarden.New(bitwarden.WithServerURL("https://vault.example.com")))
```

The token is read **lazily, at resolve time**, so a blank import is safe even
when no token is present at process start. It is also re-read on every token
refresh rather than once at construction, so a rotated `BWS_ACCESS_TOKEN` is
picked up at the next refresh instead of requiring a restart.

Both endpoints must be `https://`. The exchange POSTs the client secret in its
form body and the API returns the organization's secrets, so cleartext exposes
both; `WithAllowInsecure(true)` opts into `http://` for a local install and
permits nothing else, not even a different scheme.

`Close()` is idempotent and terminal: after it returns, every `Resolve`
reports `errors.Is(err, mamori.ErrUnavailable)` locally, without contacting
Bitwarden. It also returns its own idle HTTP connections to the pool, and
leaves connections belonging to the rest of your process alone. A client
injected with `WithHTTPClient` is never closed, so it stays usable for
whatever else holds it.

## How decryption works

Bitwarden is end to end encrypted: the API returns ciphertext and the server
cannot read it. A provider that returned what the API returned would be
returning base64, not a secret. So this package performs the full client-side
unwrap.

| Step | What happens | Standard library used |
| --- | --- | --- |
| 1 | Split the access token `0.<uuid>.<secret>:<base64 16 bytes>` | `strings`, `encoding/base64` |
| 2 | Stretch its 16-byte key to 64 bytes: HKDF-Extract with salt `bitwarden-accesstoken`, then HKDF-Expand with info `sm-access-token` | `crypto/hkdf`, `crypto/sha256` |
| 3 | `POST /connect/token` (`grant_type=client_credentials`, `scope=api.secrets`) and read `encrypted_payload` from the response | `httpcore`, `net/url` |
| 4 | Decrypt that payload with the step 2 key; it yields `{"encryptionKey":"<base64 64 bytes>"}`, the organization key | `crypto/aes`, `crypto/cipher`, `crypto/hmac` |
| 5 | `GET /secrets/<uuid>` and decrypt its `value` with the organization key | same as step 4 |

Steps 4 and 5 are the same primitive: a **type 2 EncString**,
`2.<iv>|<ciphertext>|<mac>`, each part standard base64. It is AES-256-CBC with
HMAC-SHA256 in Encrypt-then-MAC, with a 16-byte IV, a 32-byte tag, PKCS#7
padding, and a 64-byte key split into a 32-byte AES sub-key followed by a
32-byte MAC sub-key. The MAC covers `iv || ciphertext`.

**`crypto/hkdf` is what makes this possible at all.** It entered the Go
standard library in Go 1.24; before that, HKDF meant `golang.org/x/crypto`,
which this repository does not take as a dependency, and Bitwarden's own SDK
is a Rust core reached through cgo bindings, which is a categorical departure
this repository will not make either. This module requires Go 1.26.

### Security properties, and why each is written the way it is

- **The MAC is verified before the ciphertext is decrypted**, with
  `hmac.Equal` and never `bytes.Equal`. Verifying first means a forged
  ciphertext never reaches the cipher, which removes the padding-oracle class
  of attack outright: the difference between "bad padding" and "bad plaintext"
  is never computed. `bytes.Equal` returns as soon as two bytes differ, so its
  timing reveals how many leading bytes of a candidate tag were right, which is
  enough to forge one byte at a time.
- **EncString type 0 is refused, not decrypted.** `0.<iv>|<data>` is AES-CBC
  with no MAC. Bitwarden's own source marks it as a variant that must never be
  used to encrypt. Accepting it would return a value whose integrity nothing
  checked.
- **EncString type 7 is refused with a reason.** `7.<cose bytes>` is a COSE
  Encrypt0 message carrying XChaCha20-Poly1305. Neither that cipher nor CBOR
  is in the Go standard library, so this provider *cannot* decrypt it. It
  reports `mamori.ErrUnavailable` naming the missing primitive, rather than a
  parse error, because the ciphertext is well formed and the shortfall is the
  provider's. **This is the module's main forward risk**: type 7 is Bitwarden's
  preferred variant for new data, and if Secrets Manager values migrate to it,
  this provider stops working and will say so loudly rather than silently.
- **No key material sits in a readable struct field.** Every derived key, the
  organization key, the client secret, and the bearer token live inside
  closures. This is not style: `fmt`'s `%v`, `%+v` and `%#v` walk unexported
  fields by reflection, and reflection **cannot** call a `String` or `GoString`
  method on a value it reaches that way, so a redacting wrapper type would not
  have helped. Reflection renders a func value as an opaque pointer.
  [`httpcore/oauth2.go`](../httpcore/oauth2.go) established this pattern and
  its comment explains it at length.
- **Intermediates are zeroed.** The HKDF outputs, the decrypted identity
  payload, the raw organization key bytes, and the full CBC buffer including
  padding are all wiped once they have been copied onward.
- **A superseded organization key is deliberately NOT zeroed on refresh.** A
  resolve that started before the refresh may still be decrypting with it, and
  [`secret.String.Zero`](../../secret/secret.go)'s own doc comment sets out
  exactly this hazard: copies share one backing array, so wiping on rotation is
  a use after free with extra steps. mamori's core never zeroes a superseded
  value either.
- **No key, token, ciphertext, or plaintext ever reaches an error message.**
  `mamori.SelectKey` is the subtle case: it is handed the decrypted value and
  wraps `encoding/json`'s error, which quotes its input, so this package
  preserves its *classification* and discards its *text*.

## Watch

Secrets Manager exposes no push channel, so mamori polls (interval + jitter).
Configure with `mamori.WithPollInterval`. The access token is cached and
refreshed 30 seconds before expiry by default (`WithLeeway`), and concurrent
resolves starting cold share a single exchange rather than one each.

## Error classification

Both endpoints classify through
[`httpcore.ClassifyStatus`](../httpcore/README.md#error-classification):

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

A rejected token exchange also carries RFC 6749's `error` code, such as
`invalid_client`. That code is a fixed token, never free text and never derived
from the client secret, so it is the one part of a failed exchange that is safe
to surface; `error_description` is deliberately **not** read. The secrets
endpoint sets no `ErrorDetail` hook at all, which is `httpcore`'s safe default:
its bodies carry secret material and no field of its error shape has been
established as free of it.

Errors name the **machine account UUID** on purpose. It is an identifier, not a
credential, and without it a deployment with several machine accounts cannot
tell which one failed.

## What is verified

This section is precise about the difference between "matches the vendor" and
"is internally consistent", because for a crypto provider that distinction is
the whole question.

- ✅ **Vendor-verified: the key derivation.** `TestDeriveShareableKeyVendorVectors`
  asserts two test vectors copied verbatim from Bitwarden's own
  `bitwarden-crypto/src/keys/shareable_key.rs`. They match byte for byte, which
  establishes that `crypto/hkdf` reproduces `derive_shareable_key` exactly,
  including that Bitwarden's bare `HMAC-SHA256(salt, secret)` extract step is
  the same construction as RFC 5869's HKDF-Extract.
- ✅ **Vendor-verified: the cipher, padding, MAC order, and EncString format.**
  `TestDecryptVendorVector` decrypts Bitwarden's published ciphertext with
  Bitwarden's published key and asserts its published plaintext, from
  `aes256_cbc_hmac_sha256_ae.rs`. A self-consistent round trip could not
  establish any of this: it would pass just as green with the MAC computed over
  the ciphertext alone, or with the two key halves swapped.
- ✅ **Vendor-verified: the wire constants.** The access token's
  `"accesstoken"` / `"sm-access-token"` arguments are asserted against the
  literals in `bitwarden-core/src/auth/access_token.rs`, and the fake's access
  token is the canonical one from that file's own round-trip test.
- ⚠️ **Self-consistent only: the end-to-end wiring.** The in-process fake
  encrypts with its own implementation of the same scheme (`sealEncString` in
  `fake_test.go`), so the conformance and resolve tests prove the provider's
  plumbing round trips, **not** that the format is Bitwarden's. The vendor
  vectors above are what carry that claim. The two arguments together are the
  case; neither is sufficient alone.
- ❌ **Not verified against a live Bitwarden organization at authoring time.**
  No real Secrets Manager tenant was available, so **no ciphertext produced by
  Bitwarden's own servers has been decrypted by this code**. The integration
  test exists for exactly that gap and skips cleanly without credentials:

  ```bash
  export BWS_ACCESS_TOKEN=0.uuid.secret:key==
  export MAMORI_BWS_SECRET_ID=6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d
  GOWORK=off go test -tags integration ./...
  ```

  Anyone with a tenant should run it before trusting this provider in
  production.

Also verified, by mutation rather than by inspection - each of these was broken
on purpose, the named test was confirmed to fail, and the change was reverted:

- Removing the `hmac.Equal` check fails `TestDecryptRefusesTamperedCiphertext`
  (which flips every byte of the ciphertext in turn), plus three more.
- Changing the HKDF salt prefix by one character fails both vendor vectors.
- Turning `symKey`'s key closures into plain fields fails all three
  `...FormattingLeaksNothing` tests under `%v`, `%+v` and `%#v`.

That last mutation also caught two **weak assertions** in the leak tests, which
were then strengthened: the original checks looked only for base64 and the
decimal `[1 2 3]` rendering, so they missed `%#v`'s `[]uint8{0x1, ...}` form,
and they compared the concatenated 64-byte key, which never appears
contiguously because a `symKey` renders its two halves separately.

- ✅ Conformance: the [`providertest`](../../providertest) kit runs against an
  in-process `http.RoundTripper` (never an `httptest.Server`, whose accept
  goroutine the `NoGoroutineLeak` case could not tolerate). The fake checks
  `req.Context().Err()` explicitly.
- ✅ A status-to-`Kind` table test, separate from the kit.
- ✅ `go test -race` and `go vet -tags integration`.

## Development

This package is its own Go module. Run all commands with the workspace
disabled:

```sh
cd providers/bitwarden
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -race ./...
GOWORK=off go vet -tags integration ./...
```
