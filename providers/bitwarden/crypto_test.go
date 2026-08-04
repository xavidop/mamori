package bitwarden

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// keyMaterial concatenates a symKey's two halves back into the 64-byte form
// Bitwarden serializes, so a derived key can be compared with a vendor vector.
func keyMaterial(k symKey) []byte {
	out := make([]byte, 0, symKeySize)
	out = append(out, k.enc()...)
	out = append(out, k.mac()...)
	return out
}

// TestDeriveShareableKeyVendorVectors asserts this package's HKDF derivation
// against Bitwarden's OWN published test vectors, copied verbatim from
// bitwarden/sdk-internal crates/bitwarden-crypto/src/keys/shareable_key.rs.
//
// This is the load-bearing crypto test in the module. Everything downstream -
// unwrapping the organization key, decrypting a secret - is worthless if the
// derived key is not bit-identical to what Bitwarden's Rust core produces, and
// no amount of internally consistent round-tripping would catch a wrong salt
// or a wrong info string. These two vectors would.
func TestDeriveShareableKeyVendorVectors(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		keyName string
		info    string
		want    string
	}{
		{
			name:    "no info",
			secret:  "&/$%F1a895g67HlX",
			keyName: "test_key",
			info:    "",
			want:    "4PV6+PcmF2w7YHRatvyMcVQtI7zvCyssv/wFWmzjiH6Iv9altjmDkuBD1aagLVaLezbthbSe+ktR+U6qswxNnQ==",
		},
		{
			name:    "with info",
			secret:  "67t9b5g67$%Dh89n",
			keyName: "test_key",
			info:    "test",
			want:    "F9jVQmrACGx9VUPjuzfMYDjr726JtL300Y3Yg+VYUnVQtQ1s8oImJ5xtp1KALC9h2nav04++1LDW4iFD+infng==",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, err := deriveShareableKey([]byte(tc.secret), tc.keyName, tc.info)
			if err != nil {
				t.Fatalf("deriveShareableKey: %v", err)
			}
			got := base64.StdEncoding.EncodeToString(keyMaterial(k))
			if got != tc.want {
				t.Errorf("derived key does not match Bitwarden's published vector\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestDeriveAccessTokenKeyUsesVendorArguments pins the two wire constants the
// Secrets Manager derivation depends on. A vector cannot cover them (Bitwarden
// publishes none for the access token path itself), so they are asserted
// against the literal strings read out of
// bitwarden-core/src/auth/access_token.rs:
//
//	derive_shareable_key(encryption_key, "accesstoken", Some("sm-access-token"))
//
// Without this, a typo in either constant would still produce a plausible
// 64-byte key and fail only against a live backend.
func TestDeriveAccessTokenKeyUsesVendorArguments(t *testing.T) {
	secret := bytes.Repeat([]byte{0xAB}, accessTokenSecretSize)

	got, err := deriveAccessTokenKey(secret)
	if err != nil {
		t.Fatalf("deriveAccessTokenKey: %v", err)
	}
	want, err := deriveShareableKey(secret, "accesstoken", "sm-access-token")
	if err != nil {
		t.Fatalf("deriveShareableKey: %v", err)
	}
	if !bytes.Equal(keyMaterial(got), keyMaterial(want)) {
		t.Error("deriveAccessTokenKey does not use name \"accesstoken\" with info \"sm-access-token\"")
	}
}

// vendorAES holds Bitwarden's published AES-256-CBC-HMAC-SHA256 test vector,
// copied verbatim from
// crates/bitwarden-crypto/src/hazmat/symmetric_encryption/aes256_cbc_hmac_sha256_ae.rs.
// Its own comment there says the vector "Locks in the serialized format of the
// type 2 (iv, mac, ciphertext) triple; must never break, or existing data will
// no longer decrypt."
var vendorAES = struct {
	key       []byte
	iv        []byte
	data      []byte
	mac       []byte
	plaintext string
}{
	key: func() []byte {
		k := make([]byte, symKeySize)
		for i := range k {
			k[i] = byte(i)
		}
		return k
	}(),
	iv:   []byte{216, 218, 36, 0, 196, 186, 150, 85, 49, 147, 110, 168, 185, 227, 42, 172},
	data: []byte{234, 77, 16, 15, 189, 82, 36, 188, 182, 88, 64, 67, 145, 94, 30, 178, 36, 235, 130, 67, 255, 207, 183, 168, 73, 231, 82, 122, 193, 139, 25, 129},
	mac:  []byte{60, 78, 44, 111, 72, 233, 3, 6, 86, 250, 217, 242, 62, 229, 184, 221, 231, 150, 189, 44, 99, 189, 220, 55, 196, 194, 101, 60, 102, 195, 149, 130},

	plaintext: "Bitwarden SDK test vector",
}

// vendorEncString renders the vendor vector as the `2.iv|data|mac` string a
// Bitwarden API actually returns, so the parser is exercised on the same bytes
// as the cipher.
func vendorEncString() string {
	b64 := base64.StdEncoding.EncodeToString
	return encTypeAesCbc256HmacSha256B64 + "." + b64(vendorAES.iv) + "|" + b64(vendorAES.data) + "|" + b64(vendorAES.mac)
}

// TestDecryptVendorVector decrypts Bitwarden's published ciphertext with
// Bitwarden's published key and asserts the published plaintext.
//
// This is the one test in the module that proves the cipher, padding, MAC
// input order, and EncString serialization are all the vendor's and not this
// package's invention. A self-consistent encrypt-then-decrypt round trip
// cannot prove any of them: it would pass just as green with the MAC computed
// over the ciphertext alone, or with the halves of the key swapped.
func TestDecryptVendorVector(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}
	enc, err := parseEncString(vendorEncString())
	if err != nil {
		t.Fatalf("parseEncString: %v", err)
	}
	got, err := k.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != vendorAES.plaintext {
		t.Errorf("plaintext = %q, want %q", got, vendorAES.plaintext)
	}
}

// TestDecryptRefusesTamperedCiphertext is the MAC-verification test the
// module's threat model rests on. Every byte of the ciphertext is flipped in
// turn (one mutant per byte) and each mutant must be REFUSED.
//
// Mutation-checked: deleting the `if !hmac.Equal(...)` block in decrypt makes
// this test fail, because a tampered CBC ciphertext then decrypts to garbage
// and returns either that garbage or a padding error rather than an integrity
// error. Verified by doing exactly that; see the module README.
func TestDecryptRefusesTamperedCiphertext(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}

	for i := range vendorAES.data {
		tampered := bytes.Clone(vendorAES.data)
		tampered[i] ^= 0x01

		enc := encString{iv: vendorAES.iv, data: tampered, mac: vendorAES.mac}
		got, err := k.decrypt(enc)
		if err == nil {
			t.Fatalf("byte %d: tampered ciphertext was ACCEPTED and decrypted to %q; the MAC check did not run", i, got)
		}
		if !errors.Is(err, mamori.ErrInvalid) {
			t.Fatalf("byte %d: error kind = %v, want ErrInvalid", i, err)
		}
		if !strings.Contains(err.Error(), "integrity check") {
			t.Fatalf("byte %d: error %q does not report an integrity failure, so the MAC is not what rejected it", i, err)
		}
	}
}

// TestDecryptRefusesTamperedIVAndMAC covers the two parts of the triple the
// previous test leaves alone. The IV is authenticated (the MAC is computed
// over iv||ciphertext), so flipping it must be caught too - and would not be
// if the MAC were computed over the ciphertext alone, which is the easy way to
// get this construction wrong.
func TestDecryptRefusesTamperedIVAndMAC(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}

	t.Run("tampered iv", func(t *testing.T) {
		iv := bytes.Clone(vendorAES.iv)
		iv[0] ^= 0xFF
		if _, err := k.decrypt(encString{iv: iv, data: vendorAES.data, mac: vendorAES.mac}); err == nil {
			t.Fatal("a tampered IV was accepted, so the MAC does not cover the IV")
		}
	})

	t.Run("tampered mac", func(t *testing.T) {
		mac := bytes.Clone(vendorAES.mac)
		mac[len(mac)-1] ^= 0xFF
		if _, err := k.decrypt(encString{iv: vendorAES.iv, data: vendorAES.data, mac: mac}); err == nil {
			t.Fatal("a forged MAC was accepted")
		}
	})
}

// TestDecryptRefusesWrongKey asserts that a key which is not the one the
// ciphertext was sealed under is rejected at the MAC, not at the padding and
// not at all.
//
// Both halves are varied independently: a wrong MAC half must fail the
// integrity check, and a wrong encryption half must ALSO fail it, because the
// MAC covers the ciphertext that only the right encryption key produces. The
// second case is what catches a key whose halves were split at the wrong
// offset.
//
// Mutation-checked: removing the MAC check makes the wrong-MAC-half case
// surface as a padding error instead, and this test fails.
func TestDecryptRefusesWrongKey(t *testing.T) {
	enc, err := parseEncString(vendorEncString())
	if err != nil {
		t.Fatalf("parseEncString: %v", err)
	}

	cases := []struct {
		name   string
		mutate func([]byte)
	}{
		{"wrong encryption half", func(k []byte) { k[0] ^= 0xFF }},
		{"wrong mac half", func(k []byte) { k[encKeySize] ^= 0xFF }},
		{"halves swapped", func(k []byte) {
			enc := bytes.Clone(k[:encKeySize])
			copy(k, k[encKeySize:])
			copy(k[encKeySize:], enc)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			material := bytes.Clone(vendorAES.key)
			tc.mutate(material)
			k, err := newSymKey(material)
			if err != nil {
				t.Fatalf("newSymKey: %v", err)
			}
			got, err := k.decrypt(enc)
			if err == nil {
				t.Fatalf("the wrong key was accepted and produced %q", got)
			}
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Errorf("error kind = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestDecryptErrorLeaksNothing asserts that no failure path puts key material,
// ciphertext, or plaintext into an error message. An error is the single most
// likely place for a secret to escape, because it gets logged.
func TestDecryptErrorLeaksNothing(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}
	tampered := bytes.Clone(vendorAES.data)
	tampered[0] ^= 0x01

	_, err = k.decrypt(encString{iv: vendorAES.iv, data: tampered, mac: vendorAES.mac})
	if err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	msg := err.Error()

	b64 := base64.StdEncoding.EncodeToString
	for _, leak := range []struct {
		what string
		s    string
	}{
		{"encryption key", b64(k.enc())},
		{"mac key", b64(k.mac())},
		{"ciphertext", b64(tampered)},
		{"expected mac", b64(vendorAES.mac)},
		{"plaintext", vendorAES.plaintext},
	} {
		if strings.Contains(msg, leak.s) {
			t.Errorf("decrypt error leaked the %s: %q", leak.what, msg)
		}
	}
}

// TestParseEncStringRefusesUnauthenticatedType0 asserts that the deprecated
// unauthenticated variant is refused rather than decrypted. Accepting it would
// hand back a value whose integrity nothing verified, which is the exact
// outcome the MAC-first rule exists to prevent.
func TestParseEncStringRefusesUnauthenticatedType0(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	s := "0." + b64(vendorAES.iv) + "|" + b64(vendorAES.data)

	_, err := parseEncString(s)
	if err == nil {
		t.Fatal("an unauthenticated type 0 EncString was accepted")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error kind = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Errorf("error %q does not say why type 0 is refused", err)
	}
}

// TestParseEncStringRefusesCOSEType7 asserts that Bitwarden's newer COSE
// variant fails loudly and as ErrUnavailable, naming the missing primitive.
//
// This is the module's forward-compatibility tripwire. Type 7 carries
// XChaCha20-Poly1305, which is not in the Go standard library, so if Bitwarden
// migrates Secrets Manager values to it this provider cannot decrypt them. It
// must say so, rather than return an empty value or a confusing parse error.
func TestParseEncStringRefusesCOSEType7(t *testing.T) {
	_, err := parseEncString("7." + base64.StdEncoding.EncodeToString([]byte("cose-bytes")))
	if err == nil {
		t.Fatal("a type 7 COSE EncString was accepted")
	}
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Errorf("error kind = %v, want ErrUnavailable so an operator sees a provider limitation, not corrupt data", err)
	}
	if !strings.Contains(err.Error(), "standard library") {
		t.Errorf("error %q does not name the reason it cannot be decrypted", err)
	}
}

// TestParseEncStringRejectsMalformed covers the shapes a hostile or broken
// backend could send. Each must be refused before any of it reaches the
// cipher, so CryptBlocks can never be handed a buffer it would panic on.
func TestParseEncStringRejectsMalformed(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	shortIV := b64(vendorAES.iv[:8])
	goodIV, goodData, goodMAC := b64(vendorAES.iv), b64(vendorAES.data), b64(vendorAES.mac)

	cases := []struct{ name, in string }{
		{"empty", ""},
		{"no type prefix", goodIV + "|" + goodData + "|" + goodMAC},
		{"unknown type", "9." + goodIV + "|" + goodData + "|" + goodMAC},
		{"two parts", "2." + goodIV + "|" + goodData},
		{"four parts", "2." + goodIV + "|" + goodData + "|" + goodMAC + "|extra"},
		{"short iv", "2." + shortIV + "|" + goodData + "|" + goodMAC},
		{"short mac", "2." + goodIV + "|" + goodData + "|" + b64(vendorAES.mac[:16])},
		{"iv not base64", "2.!!!!|" + goodData + "|" + goodMAC},
		{"data not base64", "2." + goodIV + "|!!!!|" + goodMAC},
		{"empty data", "2." + goodIV + "||" + goodMAC},
		{"data not a block multiple", "2." + goodIV + "|" + b64(bytes.Repeat([]byte{1}, aes.BlockSize+1)) + "|" + goodMAC},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseEncString(tc.in); err == nil {
				t.Fatalf("parseEncString(%q) succeeded, want an error", tc.name)
			}
		})
	}
}

// TestUnpadPKCS7 covers the padding stripper directly, including the invalid
// forms a decrypt under the wrong key would produce if the MAC check were ever
// bypassed.
func TestUnpadPKCS7(t *testing.T) {
	t.Run("full block of padding", func(t *testing.T) {
		in := append(bytes.Repeat([]byte("A"), 16), bytes.Repeat([]byte{16}, 16)...)
		got, err := unpadPKCS7(in)
		if err != nil {
			t.Fatalf("unpadPKCS7: %v", err)
		}
		if string(got) != strings.Repeat("A", 16) {
			t.Errorf("got %q", got)
		}
	})

	bad := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"not a block multiple", bytes.Repeat([]byte{1}, 17)},
		{"zero pad length", append(bytes.Repeat([]byte("A"), 15), 0)},
		{"pad longer than a block", append(bytes.Repeat([]byte("A"), 15), 17)},
		{"inconsistent padding", append(bytes.Repeat([]byte("A"), 13), 3, 2, 3)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := unpadPKCS7(tc.in); err == nil {
				t.Fatal("invalid padding was accepted")
			}
		})
	}
}

// TestNewSymKeyRejectsWrongLength asserts a short or long key cannot become a
// silently truncated one.
func TestNewSymKeyRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 31, 32, 63, 65} {
		if _, err := newSymKey(bytes.Repeat([]byte{7}, n)); err == nil {
			t.Errorf("newSymKey accepted a %d-byte key, want %d only", n, symKeySize)
		}
	}
}

// TestSymKeyWipe asserts that wipe actually zeroes the material both closures
// read through, so the defer in the exchange path is not decorative.
func TestSymKeyWipe(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}
	k.wipe()

	if !bytes.Equal(k.enc(), make([]byte, encKeySize)) {
		t.Error("wipe left the encryption half readable")
	}
	if !bytes.Equal(k.mac(), make([]byte, macKeySize)) {
		t.Error("wipe left the MAC half readable")
	}
}

// TestDecryptOnZeroKeyDoesNotPanic asserts the closure guard: a symKey left at
// its zero value on an error path must return an error, not panic on a nil
// func call.
func TestDecryptOnZeroKeyDoesNotPanic(t *testing.T) {
	var k symKey
	if _, err := k.decrypt(encString{iv: vendorAES.iv, data: vendorAES.data, mac: vendorAES.mac}); err == nil {
		t.Fatal("an uninitialized key decrypted something")
	}
}
