package bitwarden

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// verbs are the three fmt verbs that walk unexported struct fields by
// reflection. %v and %+v render them, and %#v renders a Go-syntax literal of
// the whole graph; none of them can be intercepted by a String or GoString
// method on a value reached through reflection, which is precisely why this
// package keeps key material inside closures instead.
var verbs = []string{"%v", "%+v", "%#v"}

// byteRenderings returns every textual form fmt might print b in.
//
// Checking only one form is how a leak test passes while leaking, and this
// module found that out the hard way: an earlier version of these tests
// checked base64 and the decimal `[1 2 3]` form, so a mutant that exposed the
// key as a plain struct field went UNDETECTED under %#v, which renders a byte
// slice as `[]uint8{0x1, 0x2, ...}`. All four forms are generated now, and the
// bracketed ones are reduced to their contents so the surrounding type name
// (which differs between []byte and []uint8 depending on how fmt reached the
// value) cannot make the match miss.
func byteRenderings(b []byte) []string {
	goSyntax := fmt.Sprintf("%#v", b)
	if i := strings.Index(goSyntax, "{"); i >= 0 {
		goSyntax = strings.TrimSuffix(goSyntax[i+1:], "}")
	}
	decimal := strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%v", b), "["), "]")

	return []string{
		base64.StdEncoding.EncodeToString(b),
		decimal,
		goSyntax,
		fmt.Sprintf("%x", b),
	}
}

// secretsThatMustNotAppear lists every piece of material that would be a real
// compromise if it reached a log line, mapped to every rendering that would
// betray it.
func secretsThatMustNotAppear(t *testing.T, f *fakeBackend) map[string][]string {
	t.Helper()

	tokenSecret, err := base64.StdEncoding.DecodeString(fakeTokenKeyB64)
	if err != nil {
		t.Fatalf("decoding the fake token key: %v", err)
	}
	derived, err := deriveAccessTokenKey(tokenSecret)
	if err != nil {
		t.Fatalf("deriveAccessTokenKey: %v", err)
	}

	// Each key is listed by HALF as well as whole. A symKey holds its two
	// 32-byte halves as separate views, so a dump renders them separately and
	// the concatenated 64-byte form never appears as a contiguous substring:
	// checking only the whole key silently passes on a leak of both halves.
	// This too was found by mutation rather than by inspection.
	return map[string][]string{
		"the whole access token":           {fakeAccessToken},
		"the client secret":                {fakeClientSecret},
		"the access token key (base64)":    {fakeTokenKeyB64},
		"the bearer token":                 {"fake-bearer-token"},
		"the derived encryption key":       byteRenderings(derived.enc()),
		"the derived MAC key":              byteRenderings(derived.mac()),
		"the organization key":             byteRenderings(keyMaterial(f.orgKey)),
		"the organization encryption half": byteRenderings(f.orgKey.enc()),
		"the organization MAC half":        byteRenderings(f.orgKey.mac()),
	}
}

// assertNoLeak fails when rendered contains any form of any listed secret.
func assertNoLeak(t *testing.T, what, rendered string, leaks map[string][]string) {
	t.Helper()
	for name, forms := range leaks {
		for _, form := range forms {
			if form == "" {
				continue
			}
			if strings.Contains(rendered, form) {
				t.Errorf("%s leaked %s", what, name)
				break
			}
		}
	}
}

// TestProviderFormattingLeaksNothing is the reflection test the module's
// in-memory hygiene rests on. It resolves a secret, so every credential is
// populated and cached, and then renders the Provider and the objects hanging
// off it with all three reflecting verbs.
//
// Mutation-checked, and the check is the point of the test. Changing symKey's
// `enc func() []byte` to a plain `enc []byte` field, or session's
// `cached func() string` to a `cached string`, makes this test fail: %#v then
// prints the raw bytes. Reverting restores it. A redacting wrapper type with a
// String method does NOT fix the mutant, which is the whole reason the
// closures exist; see the comment on symKey.
func TestProviderFormattingLeaksNothing(t *testing.T) {
	f := newFake(t)
	const plaintext = "correct-horse-battery-staple"
	id := secretUUID("redaction")
	f.set(id, plaintext)

	p := f.provider()
	v, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != plaintext {
		t.Fatalf("resolved %q, want %q", v.Bytes, plaintext)
	}

	leaks := secretsThatMustNotAppear(t, f)
	// The decrypted value must not be retained anywhere reachable from the
	// provider either: it belongs to the caller and nowhere else.
	leaks["the decrypted plaintext"] = []string{plaintext}

	targets := map[string]any{
		"Provider": p,
		"session":  p.sess,
		"organization key": func() symKey {
			_, k, err := p.sess.ensure(context.Background())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			return k
		}(),
	}

	for name, target := range targets {
		for _, verb := range verbs {
			assertNoLeak(t, fmt.Sprintf("fmt.Sprintf(%q, %s)", verb, name), fmt.Sprintf(verb, target), leaks)
		}
	}
}

// TestAccessTokenFormattingLeaksNothing covers the parsed token on its own,
// including the failure mode where a caller dumps it while debugging a
// misconfigured machine account.
//
// The machine account id is deliberately NOT treated as a leak: it is an
// identifier rather than a credential, and errors name it on purpose so a
// deployment with several machine accounts can tell which one failed.
func TestAccessTokenFormattingLeaksNothing(t *testing.T) {
	tok, err := parseAccessToken(fakeAccessToken)
	if err != nil {
		t.Fatalf("parseAccessToken: %v", err)
	}

	leaks := map[string][]string{
		"the client secret":          {fakeClientSecret},
		"the whole access token":     {fakeAccessToken},
		"the derived encryption key": byteRenderings(tok.key.enc()),
		"the derived MAC key":        byteRenderings(tok.key.mac()),
	}

	for _, verb := range verbs {
		assertNoLeak(t, fmt.Sprintf("fmt.Sprintf(%q, accessToken)", verb), fmt.Sprintf(verb, tok), leaks)
	}
}

// TestSymKeyFormattingLeaksNothing pins the property on the type that carries
// it, independently of how it was built, so a future key producer inherits the
// guarantee rather than having to remember it.
func TestSymKeyFormattingLeaksNothing(t *testing.T) {
	k, err := newSymKey(vendorAES.key)
	if err != nil {
		t.Fatalf("newSymKey: %v", err)
	}
	leaks := map[string][]string{
		"the encryption half": byteRenderings(k.enc()),
		"the MAC half":        byteRenderings(k.mac()),
	}

	for _, verb := range verbs {
		assertNoLeak(t, fmt.Sprintf("fmt.Sprintf(%q, symKey)", verb), fmt.Sprintf(verb, k), leaks)
	}
}

// TestResolveErrorsLeakNothing walks the error paths a misconfiguration
// actually takes and asserts none of them carries credential material. Errors
// are the likeliest escape route because they are logged by default.
func TestResolveErrorsLeakNothing(t *testing.T) {
	f := newFake(t)
	id := secretUUID("errors")

	cases := []struct {
		name  string
		setup func(*fakeBackend) *Provider
	}{
		{
			name: "identity rejects the credentials",
			setup: func(f *fakeBackend) *Provider {
				f.identityStatus = 400
				return f.provider()
			},
		},
		{
			name: "identity omits the encrypted payload",
			setup: func(f *fakeBackend) *Provider {
				f.omitPayload = true
				return f.provider()
			},
		},
		{
			name: "encrypted payload does not match the token",
			setup: func(f *fakeBackend) *Provider {
				other := newFake(t)
				f.payloadOverride = other.sealPayloadLocked()
				// Re-seal under a key the provider's token cannot derive.
				bogus, err := deriveShareableKey([]byte("0123456789abcdef"), "accesstoken", "sm-access-token")
				if err != nil {
					t.Fatalf("deriveShareableKey: %v", err)
				}
				f.payloadOverride = sealEncString(t, bogus, []byte(`{"encryptionKey":"AAAA"}`))
				return f.provider()
			},
		},
		{
			name:  "secret is missing",
			setup: func(f *fakeBackend) *Provider { return f.provider() },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFake(t)
			p := tc.setup(fb)
			_, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
			if err == nil {
				t.Fatal("expected an error")
			}
			assertNoLeak(t, "the error message", err.Error(), secretsThatMustNotAppear(t, fb))
		})
	}
	_ = f
}

// mustRef parses a ref or fails the test.
func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}
