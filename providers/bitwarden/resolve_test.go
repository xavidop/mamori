package bitwarden

import (
	"context"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveReturnsDecryptedValue is the end-to-end assertion that the
// provider returns PLAINTEXT and not the ciphertext it was handed. It is
// stated on its own, and checked against the ciphertext explicitly, because
// returning base64 and calling it a value is the specific failure this whole
// module exists to avoid.
func TestResolveReturnsDecryptedValue(t *testing.T) {
	f := newFake(t)
	id := secretUUID("plaintext")
	const want = "sk_live_not_a_real_key"
	f.set(id, want)

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != want {
		t.Fatalf("Resolve returned %q, want %q", v.Bytes, want)
	}
	if strings.HasPrefix(string(v.Bytes), "2.") {
		t.Fatal("Resolve returned an EncString rather than the decrypted value")
	}
}

// TestResolveMarksValuesSensitive asserts every value is Sensitive. Unlike a
// generic HTTP endpoint, which may serve configuration or secrets, everything
// Secrets Manager holds is a secret by construction, so this is not a
// per-endpoint decision.
func TestResolveMarksValuesSensitive(t *testing.T) {
	f := newFake(t)
	id := secretUUID("sensitive")
	f.set(id, "value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive is false; downstream redaction depends on it")
	}
}

// TestVersionTracksRevisionDate asserts the Version comes from the plaintext
// revisionDate field and changes when the secret does.
//
// It also asserts the Version is not the decrypted value, which a hash-based
// implementation could accidentally make it: a Version is held and compared by
// the reconciler and may be logged, so it must never be the secret.
func TestVersionTracksRevisionDate(t *testing.T) {
	f := newFake(t)
	id := secretUUID("version")
	const first = "one"
	f.set(id, first)

	p := f.provider()
	ref := mustRef(t, "bitwarden-sm://"+id)

	v1, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == "" {
		t.Fatal("Version is empty")
	}
	if strings.Contains(v1.Version, first) {
		t.Error("Version carries the decrypted value")
	}

	// Rewriting the same value still bumps the revision, which is what the
	// backend does and what the reconciler must tolerate.
	f.set(id, "two")
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == v2.Version {
		t.Errorf("Version did not change after a write: %q", v2.Version)
	}
}

// TestFragmentSelectsFromDecryptedValue asserts a #fragment is applied to the
// PLAINTEXT, which is the only place it could sensibly apply, and behaves like
// mamori.SelectKey everywhere else.
func TestFragmentSelectsFromDecryptedValue(t *testing.T) {
	f := newFake(t)
	id := secretUUID("fragment")
	f.set(id, `{"username":"svc","password":"hunter2","nested":{"deep":"value"}}`)

	p := f.provider()
	cases := []struct{ frag, want string }{
		{"#password", "hunter2"},
		{"#username", "svc"},
		{"#/nested/deep", "value"},
	}
	for _, tc := range cases {
		t.Run(tc.frag, func(t *testing.T) {
			v, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id+tc.frag))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if string(v.Bytes) != tc.want {
				t.Errorf("got %q, want %q", v.Bytes, tc.want)
			}
		})
	}
}

// TestFragmentMissLeaksNoPlaintext asserts a failed selection reports the kind
// and the selector but never the document it searched. mamori.SelectKey's own
// error wraps encoding/json's, which quotes its input, so this is the path
// that would otherwise put the secret in a log line.
func TestFragmentMissLeaksNoPlaintext(t *testing.T) {
	f := newFake(t)
	id := secretUUID("fragment-miss")
	const plaintext = `{"username":"svc","password":"hunter2"}`
	f.set(id, plaintext)

	p := f.provider()

	// A key that is absent from a JSON object is not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id+"#absent"))
	if err == nil {
		t.Fatal("an absent key resolved successfully")
	}
	if mamori.ErrorKind(err) != mamori.KindNotFound {
		t.Errorf("absent key classified as %v, want not_found", mamori.ErrorKind(err))
	}
	for _, leak := range []string{"hunter2", plaintext} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("selector error leaked plaintext: %v", err)
		}
	}

	// A selector applied to a value that is not a JSON object is invalid, not
	// not-found: not-found would make mamori apply the field default.
	f.set(id, "just-a-string")
	_, err = p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id+"#anything"))
	if err == nil {
		t.Fatal("a selector on a non-object resolved successfully")
	}
	if mamori.ErrorKind(err) != mamori.KindInvalid {
		t.Errorf("selector on a non-object classified as %v, want invalid", mamori.ErrorKind(err))
	}
	if strings.Contains(err.Error(), "just-a-string") {
		t.Errorf("selector error leaked plaintext: %v", err)
	}
}

// TestResolveSendsBearerToken asserts the API request actually carries the
// token the exchange bought, on the header Bitwarden expects.
func TestResolveSendsBearerToken(t *testing.T) {
	f := newFake(t)
	id := secretUUID("bearer")
	f.set(id, "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.mu.Lock()
	auth, path := f.lastAuth, f.lastPath
	f.mu.Unlock()

	if auth != "Bearer fake-bearer-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer fake-bearer-token")
	}
	if path != "/secrets/"+id {
		t.Errorf("request path = %q, want %q", path, "/secrets/"+id)
	}
}

// TestSchemeIsRegistered asserts the scheme string is what the docs, the
// sourcetag table, and every ref in a struct tag depend on.
func TestSchemeIsRegistered(t *testing.T) {
	if got := New().Scheme(); got != "bitwarden-sm" {
		t.Errorf("Scheme() = %q, want %q", got, "bitwarden-sm")
	}
}

// TestSecretWithoutValueIsNotFound asserts a response missing its value field
// is reported rather than resolving to an empty secret. An empty string
// applied as though it were the value is the worst possible outcome here,
// because it would authenticate as a blank credential.
func TestSecretWithoutValueIsNotFound(t *testing.T) {
	f := newFake(t)
	id := secretUUID("novalue")
	// Seed nothing, so the fake 404s, which is the reachable form of this.
	_, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
	if mamori.ErrorKind(err) != mamori.KindNotFound {
		t.Errorf("err = %v, want not_found", err)
	}
}
