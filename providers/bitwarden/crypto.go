package bitwarden

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/xavidop/mamori"
)

// EncString type prefixes, the leading digit of a Bitwarden ciphertext string.
//
// Only type 2 is decryptable here, and the other two are named so they can be
// refused with a message that says which one arrived rather than a generic
// parse failure. Bitwarden's own enum documents 1 as removed, so a gap between
// 0 and 2 is expected rather than an omission.
const (
	// encTypeAesCbc256B64 is `0.iv|data`: AES-256-CBC with NO authentication.
	// Bitwarden's own source marks it "Deprecated and MUST NOT be used for
	// encrypting as it is not authenticated".
	encTypeAesCbc256B64 = "0"
	// encTypeAesCbc256HmacSha256B64 is `2.iv|data|mac`: AES-256-CBC with
	// HMAC-SHA256 in Encrypt-then-MAC. This is what Secrets Manager returns.
	encTypeAesCbc256HmacSha256B64 = "2"
	// encTypeCoseEncrypt0B64 is `7.cose_bytes`: a COSE Encrypt0 message,
	// currently carrying XChaCha20-Poly1305. It is Bitwarden's preferred
	// variant for NEW data and is not implementable here; see parseEncString.
	encTypeCoseEncrypt0B64 = "7"
)

// Sizes of the fixed-width pieces of a type 2 EncString and of the symmetric
// key that opens one. Bitwarden's Rust core declares the same five constants.
const (
	ivSize     = aes.BlockSize // 16, the AES-CBC initialization vector
	macSize    = sha256.Size   // 32, an HMAC-SHA256 tag
	encKeySize = 32            // AES-256 encryption sub-key
	macKeySize = 32            // HMAC-SHA256 authentication sub-key
	symKeySize = encKeySize + macKeySize
)

// The two arguments Bitwarden's derive_shareable_key is called with for a
// Secrets Manager access token. They are wire constants: a single wrong
// character yields a key that decrypts nothing, with no other symptom, which
// is why they are named here and asserted against the vendor's own test
// vectors rather than spelled inline.
const (
	// accessTokenKeyName becomes the HKDF-Extract salt "bitwarden-accesstoken".
	accessTokenKeyName = "accesstoken"
	// accessTokenKeyInfo is the HKDF-Expand info string.
	accessTokenKeyInfo = "sm-access-token"
)

// symKey is a Bitwarden 64-byte symmetric key: a 32-byte AES-256 encryption
// sub-key followed by a 32-byte HMAC-SHA256 authentication sub-key.
//
// Both halves are reachable only through closures, never through a field, and
// that is a security property rather than a style choice. fmt's %v, %+v and
// %#v walk a struct's unexported fields by reflection, and reflection cannot
// call a String or GoString method on a value it reaches that way, so a
// []byte field here would print as raw key bytes from any debug dump, log
// line, or panic trace, and NO redacting wrapper type could prevent it.
// Reflection renders a func value as an opaque pointer and cannot reach what
// it closed over. providers/httpcore/oauth2.go establishes this pattern and
// its comment on oauth2Auth explains it at length; a derived decryption key
// earns the same treatment as the bearer token that package protects.
type symKey struct {
	enc func() []byte
	mac func() []byte
	// wipe zeroes the single allocation both closures read through. It is a
	// closure for the same reason they are: it must close over that allocation
	// without naming it in a field.
	wipe func()
}

// newSymKey copies material into a key whose halves are reachable only through
// closures. The caller keeps ownership of material and should wipe it.
//
// The length is validated here rather than at the call sites because both
// producers of a symKey (the access-token derivation and the organization key
// from the identity payload) can fail the same way, and a short key must never
// reach aes.NewCipher as a silently truncated one.
func newSymKey(material []byte) (symKey, error) {
	if len(material) != symKeySize {
		// The length is safe to report. The bytes are not, and are not.
		return symKey{}, fmt.Errorf("bitwarden: symmetric key is %d bytes, want %d: %w",
			len(material), symKeySize, mamori.ErrInvalid)
	}
	buf := make([]byte, symKeySize)
	copy(buf, material)
	return symKey{
		enc:  func() []byte { return buf[:encKeySize] },
		mac:  func() []byte { return buf[encKeySize:] },
		wipe: func() { clear(buf) },
	}, nil
}

// valid reports whether k was built by newSymKey rather than left at its zero
// value, so a caller cannot invoke a nil closure on an error path.
func (k symKey) valid() bool { return k.enc != nil }

// deriveShareableKey reproduces Bitwarden's derive_shareable_key: HKDF-Extract
// with the salt "bitwarden-<name>" over secret, then RFC 5869 HKDF-Expand with
// info to 64 bytes, both over SHA-256.
//
// Bitwarden's Rust performs the extract step as a bare
// HMAC-SHA256(key: salt, msg: secret) rather than by calling an HKDF extract
// helper. That is not a different construction: RFC 5869 defines
// HKDF-Extract(salt, IKM) as exactly that HMAC, so crypto/hkdf.Extract
// produces the same pseudorandom key. The function is written in this general
// form, rather than with the access token's two arguments baked in, so that
// Bitwarden's own published derive_shareable_key test vectors - which use a
// different name and info - can be asserted against it directly. They are, in
// crypto_test.go, and they match byte for byte.
//
// secret is not retained; the caller still owns and should wipe it.
func deriveShareableKey(secret []byte, name, info string) (symKey, error) {
	prk, err := hkdf.Extract(sha256.New, secret, []byte("bitwarden-"+name))
	if err != nil {
		// No input is echoed: the inputs are key material and a constant.
		return symKey{}, fmt.Errorf("bitwarden: key derivation failed at HKDF-Extract: %w: %w",
			mamori.ErrInvalid, err)
	}
	defer clear(prk)

	material, err := hkdf.Expand(sha256.New, prk, info, symKeySize)
	if err != nil {
		return symKey{}, fmt.Errorf("bitwarden: key derivation failed at HKDF-Expand: %w: %w",
			mamori.ErrInvalid, err)
	}
	defer clear(material)

	return newSymKey(material)
}

// deriveAccessTokenKey derives the key that opens the identity endpoint's
// encrypted payload from the 16 bytes carried after the ':' in a machine
// account access token.
func deriveAccessTokenKey(secret []byte) (symKey, error) {
	return deriveShareableKey(secret, accessTokenKeyName, accessTokenKeyInfo)
}

// encString is a parsed type 2 EncString: `2.<iv>|<data>|<mac>`, each part
// standard base64.
//
// Only the type 2 shape is modeled. The unauthenticated type 0 and the COSE
// type 7 are refused during parsing, so no other variant can reach a field
// here and be treated as if it had a MAC.
type encString struct {
	iv   []byte
	data []byte
	mac  []byte
}

// parseEncString parses one Bitwarden ciphertext string.
//
// Two variants are refused rather than decrypted, and both refusals are
// deliberate:
//
// Type 0 (`0.iv|data`) is AES-CBC with no MAC. Accepting it would mean
// returning a value whose integrity nothing checked, which is precisely what
// the MAC-before-decrypt rule exists to prevent, and it would hand an attacker
// who can rewrite a response a CBC malleability primitive over the plaintext.
// Bitwarden itself marks the variant as one that must never be used to
// encrypt, so refusing it costs nothing a live Secrets Manager relies on.
//
// Type 7 (`7.cose_bytes`) is a COSE Encrypt0 message carrying
// XChaCha20-Poly1305. Neither XChaCha20-Poly1305 nor CBOR is in the Go
// standard library, and this module is standard library only, so it CANNOT be
// decrypted here. It is reported as mamori.ErrUnavailable with the reason
// named, rather than as a parse error, because the ciphertext is well formed
// and the shortfall is this provider's: an operator reading the message needs
// to know the value is intact and the provider cannot open it.
//
// The ciphertext is never echoed into an error, so a failure message cannot
// carry the encrypted value or its length-revealing prefix.
func parseEncString(s string) (encString, error) {
	encType, rest, ok := strings.Cut(s, ".")
	if !ok {
		return encString{}, fmt.Errorf("bitwarden: ciphertext carries no EncString type prefix: %w", mamori.ErrInvalid)
	}

	switch encType {
	case encTypeAesCbc256HmacSha256B64:
	case encTypeAesCbc256B64:
		return encString{}, fmt.Errorf(
			"bitwarden: ciphertext is EncString type 0 (AES-256-CBC with no HMAC), which is unauthenticated and is refused rather than decrypted: %w",
			mamori.ErrInvalid)
	case encTypeCoseEncrypt0B64:
		return encString{}, fmt.Errorf(
			"bitwarden: ciphertext is EncString type 7 (COSE Encrypt0, XChaCha20-Poly1305), which this provider cannot decrypt because neither that cipher nor CBOR is in the Go standard library: %w",
			mamori.ErrUnavailable)
	default:
		return encString{}, fmt.Errorf("bitwarden: unsupported EncString type %q: %w", encType, mamori.ErrInvalid)
	}

	parts := strings.Split(rest, "|")
	if len(parts) != 3 {
		return encString{}, fmt.Errorf(
			"bitwarden: type 2 EncString has %d '|'-separated parts, want 3 (iv|data|mac): %w",
			len(parts), mamori.ErrInvalid)
	}

	iv, err := decodeFixed(parts[0], ivSize, "iv")
	if err != nil {
		return encString{}, err
	}
	mac, err := decodeFixed(parts[2], macSize, "mac")
	if err != nil {
		return encString{}, err
	}

	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		// The decoder's error quotes the offending input, which here is the
		// ciphertext, so it is dropped rather than wrapped.
		return encString{}, fmt.Errorf("bitwarden: EncString data is not valid base64: %w", mamori.ErrInvalid)
	}
	// A CBC ciphertext is a whole number of blocks, and an empty one decrypts
	// to nothing. Both are checked here so CryptBlocks cannot panic on a
	// short buffer.
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return encString{}, fmt.Errorf(
			"bitwarden: EncString data is %d bytes, want a non-zero multiple of the %d-byte AES block: %w",
			len(data), aes.BlockSize, mamori.ErrInvalid)
	}

	return encString{iv: iv, data: data, mac: mac}, nil
}

// decodeFixed decodes a base64 part that must be exactly want bytes, naming the
// part in any error. The input is never echoed: these parts sit beside the
// ciphertext in the same string and an error message is not the place for it.
func decodeFixed(s string, want int, what string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: EncString %s is not valid base64: %w", what, mamori.ErrInvalid)
	}
	if len(b) != want {
		return nil, fmt.Errorf("bitwarden: EncString %s is %d bytes, want %d: %w", what, len(b), want, mamori.ErrInvalid)
	}
	return b, nil
}

// decrypt verifies the ciphertext's MAC and only then decrypts it, returning
// freshly allocated plaintext the caller owns.
//
// The order is the point. Bitwarden's type 2 EncString is Encrypt-then-MAC, so
// the MAC authenticates the (iv, ciphertext) pair and can be checked without
// touching the cipher. Verifying first means a forged or tampered ciphertext
// never reaches aes, which removes the padding-oracle class of attack outright:
// an attacker cannot learn anything from the difference between "bad padding"
// and "bad plaintext" because neither answer is ever computed. Decrypting first
// and checking afterwards would compute both.
//
// The comparison uses hmac.Equal, never bytes.Equal. bytes.Equal returns as
// soon as two bytes differ, so the time it takes reveals how many leading bytes
// of a candidate MAC were correct, which is enough to forge one byte at a time.
// hmac.Equal is constant time over the whole tag.
func (k symKey) decrypt(c encString) ([]byte, error) {
	if !k.valid() {
		return nil, fmt.Errorf("bitwarden: decryption key is not initialized: %w", mamori.ErrInvalid)
	}

	m := hmac.New(sha256.New, k.mac())
	m.Write(c.iv)
	m.Write(c.data)
	sum := m.Sum(nil)
	defer clear(sum)

	if !hmac.Equal(sum, c.mac) {
		// Neither MAC is reported. The expected tag is key-derived material and
		// the received one is attacker-chosen; echoing either turns the error
		// message into an oracle.
		return nil, fmt.Errorf(
			"bitwarden: ciphertext failed its HMAC-SHA256 integrity check, so it was tampered with or the key is wrong: %w",
			mamori.ErrInvalid)
	}

	block, err := aes.NewCipher(k.enc())
	if err != nil {
		return nil, fmt.Errorf("bitwarden: AES-256 cipher: %w: %w", mamori.ErrInvalid, err)
	}

	// The full decrypted buffer, padding included, is wiped before returning,
	// and the caller receives a tight copy of just the plaintext.
	buf := make([]byte, len(c.data))
	defer clear(buf)
	cipher.NewCBCDecrypter(block, c.iv).CryptBlocks(buf, c.data)

	unpadded, err := unpadPKCS7(buf)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(unpadded))
	copy(out, unpadded)
	return out, nil
}

// unpadPKCS7 strips PKCS#7 padding from a decrypted CBC buffer.
//
// Every padding byte is checked, not just the length byte, and the check
// accumulates differences instead of returning early. That is defense in depth
// rather than a live requirement: decrypt verifies the MAC first, so a chosen
// ciphertext never reaches this function and there is no padding oracle to
// leak through. The constant-time shape costs nothing and keeps the property
// true if this ever runs anywhere else.
//
// The buffer's contents are never echoed into an error; it is plaintext.
func unpadPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 || len(b)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bitwarden: decrypted buffer is %d bytes, not a whole number of AES blocks: %w",
			len(b), mamori.ErrInvalid)
	}
	padLen := int(b[len(b)-1])
	if padLen == 0 || padLen > aes.BlockSize || padLen > len(b) {
		return nil, errBadPadding()
	}
	var diff byte
	for _, c := range b[len(b)-padLen:] {
		diff |= c ^ byte(padLen)
	}
	if diff != 0 {
		return nil, errBadPadding()
	}
	return b[:len(b)-padLen], nil
}

// errBadPadding builds the single padding error, so the two call sites in
// unpadPKCS7 are indistinguishable to a caller reading the message.
func errBadPadding() error {
	return fmt.Errorf("bitwarden: decrypted buffer has invalid PKCS#7 padding: %w", mamori.ErrInvalid)
}
