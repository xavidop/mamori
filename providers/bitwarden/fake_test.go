package bitwarden

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixed machine account credentials the fake accepts. The 16-byte key is
// the input to the same HKDF derivation the provider performs, so the fake and
// the provider arrive at the identical payload key without either being told
// the answer.
const (
	fakeClientID     = "ec2c1d46-6a4b-4751-a310-af9601317f2d"
	fakeClientSecret = "C2IgxjjLF7qSshsbwe8JGcbM075YXw"
	fakeTokenKeyB64  = "X8vbvA0bduihIDe/qrzIQQ=="
)

// fakeAccessToken is a well-formed BWS_ACCESS_TOKEN for the fake. Its shape is
// taken from the round-trip test in Bitwarden's own
// bitwarden-core/src/auth/access_token.rs, so the parser is exercised against
// a token the vendor itself treats as canonical.
const fakeAccessToken = "0." + fakeClientID + "." + fakeClientSecret + ":" + fakeTokenKeyB64

// sealEncString encrypts plaintext under k and renders it as the type 2
// EncString a Bitwarden API returns: `2.<iv>|<ciphertext>|<mac>`.
//
// This is the fixture generator, and it is deliberately the ONLY place in the
// tests that encrypts. It is written against the format independently of
// crypto.go so that the round trip is a real round trip, but it proves only
// self-consistency: it cannot establish that the format is Bitwarden's. That
// is what the vendor vectors in crypto_test.go are for, and the two together
// are the whole argument. See the README's "What is verified".
func sealEncString(tb testing.TB, k symKey, plaintext []byte) string {
	tb.Helper()

	iv := make([]byte, ivSize)
	if _, err := rand.Read(iv); err != nil {
		tb.Fatalf("rand: %v", err)
	}

	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte{byte(padLen)}, padLen)...)

	block, err := aes.NewCipher(k.enc())
	if err != nil {
		tb.Fatalf("aes: %v", err)
	}
	data := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, padded)

	m := hmac.New(sha256.New, k.mac())
	m.Write(iv)
	m.Write(data)

	b64 := base64.StdEncoding.EncodeToString
	return "2." + b64(iv) + "|" + b64(data) + "|" + b64(m.Sum(nil))
}

// fakeSecret is one stored secret: its plaintext value and the revision that
// changes whenever it is written.
type fakeSecret struct {
	value    string
	revision string
}

// fakeBackend is an in-process Bitwarden Secrets Manager. An httptest.Server is
// deliberately not used: the conformance kit's NoGoroutineLeak case runs
// goleak with a snapshot taken before any subtest, and a live server's accept
// goroutine could never satisfy it.
type fakeBackend struct {
	mu sync.Mutex
	// orgKey is the organization key the fake seals every secret under and
	// hands out, encrypted, in the identity payload.
	orgKey     symKey
	secrets    map[string]fakeSecret
	failStatus int
	seq        int
	exchanges  int
	lastAuth   string
	lastPath   string
	// identityStatus, when non-zero, fails the token exchange instead.
	identityStatus int
	// payloadOverride replaces the encrypted_payload when non-empty.
	payloadOverride string
	// omitPayload drops the encrypted_payload field entirely.
	omitPayload bool
}

// newFake returns a backend holding a fresh random organization key.
func newFake(tb testing.TB) *fakeBackend {
	tb.Helper()
	material := make([]byte, symKeySize)
	if _, err := rand.Read(material); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	k, err := newSymKey(material)
	if err != nil {
		tb.Fatalf("newSymKey: %v", err)
	}
	return &fakeBackend{orgKey: k, secrets: map[string]fakeSecret{}}
}

// set writes a secret's plaintext and advances its revisionDate, so a poll
// sees a new Version.
func (f *fakeBackend) set(id, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.secrets[id] = fakeSecret{
		value:    val,
		revision: time.Unix(0, int64(f.seq)).UTC().Format(time.RFC3339Nano),
	}
}

// fail makes the secrets endpoint answer status until clearFail.
func (f *fakeBackend) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// clearFail cancels fail.
func (f *fakeBackend) clearFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = 0
}

// exchangeCount reports how many token exchanges the fake has served, so a
// test can assert the session cached one rather than repeating it.
func (f *fakeBackend) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// transport returns an http.RoundTripper serving both endpoints.
func (f *fakeBackend) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// Honor cancellation explicitly. http.Client delegates context
		// handling to the transport, and a real net/http.Transport notices a
		// dead context deep in its dial and connection-reuse machinery. This
		// in-process fake has none of that to fall back on, so skipping the
		// check would answer a cancelled request as though it had succeeded
		// and hide a provider that forgot to thread ctx through.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}

		switch req.URL.Host {
		case "identity.test":
			return f.serveIdentity(req)
		case "api.test":
			return f.serveAPI(req)
		default:
			return newResp(http.StatusNotFound, nil), nil
		}
	})
}

// serveIdentity implements POST /connect/token.
func (f *fakeBackend) serveIdentity(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form := string(body)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges++

	if f.identityStatus != 0 {
		return newResp(f.identityStatus, []byte(`{"error":"invalid_client"}`)), nil
	}
	if req.Method != http.MethodPost || req.URL.Path != "/connect/token" {
		return newResp(http.StatusNotFound, nil), nil
	}
	// The exchange must carry exactly what Bitwarden's AccessTokenRequest
	// carries. Asserting it here is what proves the provider sends the right
	// grant, scope, and credentials rather than merely something the fake
	// happens to accept.
	for _, want := range []string{
		"grant_type=client_credentials",
		"scope=api.secrets",
		"client_id=" + fakeClientID,
		"client_secret=" + fakeClientSecret,
	} {
		if !strings.Contains(form, want) {
			return newResp(http.StatusBadRequest, []byte(`{"error":"invalid_request"}`)), nil
		}
	}

	payload := f.payloadOverride
	if payload == "" {
		payload = f.sealPayloadLocked()
	}

	resp := map[string]any{
		"access_token": "fake-bearer-token",
		"expires_in":   3600,
		"token_type":   "Bearer",
		"scope":        "api.secrets",
	}
	if !f.omitPayload {
		resp["encrypted_payload"] = payload
	}
	b, _ := json.Marshal(resp)
	return newResp(http.StatusOK, b), nil
}

// sealPayloadLocked builds the encrypted_payload carrying the organization
// key, sealed under the key derived from the fake's own access token. The
// caller holds f.mu.
func (f *fakeBackend) sealPayloadLocked() string {
	secret, err := base64.StdEncoding.DecodeString(fakeTokenKeyB64)
	if err != nil {
		panic("fake: token key is not base64: " + err.Error())
	}
	tokenKey, err := deriveAccessTokenKey(secret)
	if err != nil {
		panic("fake: deriving the token key: " + err.Error())
	}
	claims, err := json.Marshal(payloadClaims{
		EncryptionKey: base64.StdEncoding.EncodeToString(keyMaterial(f.orgKey)),
	})
	if err != nil {
		panic("fake: marshaling claims: " + err.Error())
	}
	return sealEncString(fakeTB{}, tokenKey, claims)
}

// serveAPI implements GET /secrets/<id>.
func (f *fakeBackend) serveAPI(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastAuth = req.Header.Get("Authorization")
	f.lastPath = req.URL.EscapedPath()

	if f.failStatus != 0 {
		return newResp(f.failStatus, nil), nil
	}
	id, ok := strings.CutPrefix(req.URL.Path, "/secrets/")
	if !ok {
		return newResp(http.StatusNotFound, nil), nil
	}
	sec, ok := f.secrets[id]
	if !ok {
		return newResp(http.StatusNotFound, nil), nil
	}

	b, _ := json.Marshal(map[string]any{
		"object":         "secret",
		"id":             id,
		"organizationId": "11111111-2222-3333-4444-555555555555",
		"key":            sealEncString(fakeTB{}, f.orgKey, []byte("secret-name")),
		"value":          sealEncString(fakeTB{}, f.orgKey, []byte(sec.value)),
		"note":           sealEncString(fakeTB{}, f.orgKey, []byte("")),
		"creationDate":   "2024-01-01T00:00:00Z",
		"revisionDate":   sec.revision,
	})
	return newResp(http.StatusOK, b), nil
}

// fakeTB satisfies the testing.TB seam sealEncString takes, for the two call
// sites inside the RoundTripper where no *testing.T is in scope. A failure
// there is a bug in the fake itself, so panicking is the right response.
type fakeTB struct{ testing.TB }

func (fakeTB) Helper() {}
func (fakeTB) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newResp builds a JSON response.
func newResp(status int, body []byte) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// provider builds a Provider wired to this backend over the fake transport.
// Both URLs stay https://, so no AllowInsecure escape hatch is exercised
// merely to run the tests.
func (f *fakeBackend) provider() *Provider {
	return New(
		WithAccessToken(fakeAccessToken),
		WithIdentityURL("https://identity.test"),
		WithAPIURL("https://api.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)
}

// secretUUID maps a logical test name onto a stable UUID, because this
// provider addresses secrets by id and the conformance kit names them
// arbitrarily. Config.Key exists for exactly this translation.
func secretUUID(name string) string {
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
