package authjwt_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/x/authjwt"
)

// hmacSecret is used by every HMAC-keyed test. It is long enough to be a
// plausible real secret, which matters for the alg-confusion test where it
// doubles as PEM bytes.
var hmacSecret = []byte("test-hmac-secret-do-not-use-in-production-01234567890")

func requestWithAuthorization(t *testing.T, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if value != "" {
		r.Header.Set("Authorization", value)
	}
	return r
}

func bearerRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	return requestWithAuthorization(t, "Bearer "+token)
}

// assertUnauthenticated checks that err is non-nil, and specifically not
// mamori.ErrForbidden: a missing/invalid/expired token must produce a plain
// 401, never mamori's 403 sentinel.
func assertUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Authenticate: got nil error, want an error")
	}
	if errors.Is(err, mamori.ErrForbidden) {
		t.Fatalf("Authenticate: got mamori.ErrForbidden, want a plain (401) error: %v", err)
	}
}

func signHMAC(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(hmacSecret)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return tok
}

func baseClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "user-123",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
}

func TestValidHMACTokenAuthenticates(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:    authjwt.HMAC(hmacSecret),
		Claims: []string{"groups", "scope"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	claims["groups"] = []string{"admin", "dev"}
	claims["scope"] = "read write"
	token := signHMAC(t, claims)

	id, err := auth.Authenticate(bearerRequest(t, token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", id.Subject, "user-123")
	}
	if got, want := id.Attrs["groups"], []string{"admin", "dev"}; !equalStrings(got, want) {
		t.Errorf("Attrs[groups] = %v, want %v", got, want)
	}
	if got, want := id.Attrs["scope"], []string{"read", "write"}; !equalStrings(got, want) {
		t.Errorf("Attrs[scope] = %v, want %v (space-delimited scope must split)", got, want)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := jwt.MapClaims{
		"sub": "user-123",
		"exp": time.Now().Add(-time.Hour).Unix(),
	}
	token := signHMAC(t, claims)

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

func TestTokenWithoutExpRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := jwt.MapClaims{"sub": "user-123"}
	token := signHMAC(t, claims)

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

func TestWrongIssuerRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:    authjwt.HMAC(hmacSecret),
		Issuer: "https://issuer.example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	claims["iss"] = "https://someone-else.example.com"
	token := signHMAC(t, claims)

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

func TestMatchingIssuerAccepted(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:    authjwt.HMAC(hmacSecret),
		Issuer: "https://issuer.example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	claims["iss"] = "https://issuer.example.com"
	token := signHMAC(t, claims)

	if _, err := auth.Authenticate(bearerRequest(t, token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestWrongAudienceRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:       authjwt.HMAC(hmacSecret),
		Audiences: []string{"mamori-admin", "mamori-config"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	claims["aud"] = "some-other-service"
	token := signHMAC(t, claims)

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

func TestMatchingAudienceAccepted(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:       authjwt.HMAC(hmacSecret),
		Audiences: []string{"mamori-admin", "mamori-config"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	// The token lists several audiences; only one needs to match.
	claims["aud"] = []string{"some-other-service", "mamori-config"}
	token := signHMAC(t, claims)

	if _, err := auth.Authenticate(bearerRequest(t, token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

// TestAlgNoneRejected crafts a token signed with the "none" algorithm - the
// classic JWT footgun where a library that trusts the token's own "alg"
// header can be tricked into skipping signature verification entirely. The
// algorithm allowlist must reject it regardless of what key is configured.
func TestAlgNoneRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

// TestAlgConfusionRejected is the RSA/HMAC key-confusion attack: an RSA
// public key is configured (public, and therefore known to any attacker who
// can reach the endpoint), and the presented token is signed HS256 using the
// PEM encoding of that same public key as the HMAC secret - the forgery an
// attacker can produce without ever seeing the RSA private key. It must be
// rejected because HS256 is not among the algorithms RSAPublicKey allows.
func TestAlgConfusionRejected(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	auth, err := authjwt.New(authjwt.Config{Key: authjwt.RSAPublicKey(&priv.PublicKey)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	claims := baseClaims()
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(pubPEM)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = auth.Authenticate(bearerRequest(t, forged))
	assertUnauthenticated(t, err)
}

func TestMissingOrMalformedAuthorizationHeaderRejected(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"no header", ""},
		{"basic scheme", "Basic dXNlcjpwYXNz"},
		{"empty bearer token", "Bearer "},
		{"empty bearer token no trailing space", "Bearer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.Authenticate(requestWithAuthorization(t, tc.value))
			assertUnauthenticated(t, err)
		})
	}
}

func TestChallengeReturnsBearer(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, ok := auth.(mamori.Challenger)
	if !ok {
		t.Fatal("authenticator does not implement mamori.Challenger")
	}
	if got, want := ch.Challenge(), "Bearer"; got != want {
		t.Errorf("Challenge() = %q, want %q", got, want)
	}
}

func TestChallengeReturnsBearerWithRealm(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret), Realm: "mamori"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, ok := auth.(mamori.Challenger)
	if !ok {
		t.Fatal("authenticator does not implement mamori.Challenger")
	}
	if got, want := ch.Challenge(), `Bearer realm="mamori"`; got != want {
		t.Errorf("Challenge() = %q, want %q", got, want)
	}
}

func TestNewRejectsConfigWithoutKey(t *testing.T) {
	if _, err := authjwt.New(authjwt.Config{}); err == nil {
		t.Fatal("New: got nil error for a Config with no key source, want an error")
	}
}

func TestNewRejectsRawKeyfuncWithoutAlgorithms(t *testing.T) {
	kf := func(t *jwt.Token) (any, error) { return hmacSecret, nil }
	if _, err := authjwt.New(authjwt.Config{Keyfunc: kf}); err == nil {
		t.Fatal("New: got nil error for a Keyfunc without Algorithms, want an error")
	}
}

func TestNewRejectsBothKeyAndKeyfunc(t *testing.T) {
	kf := func(t *jwt.Token) (any, error) { return hmacSecret, nil }
	_, err := authjwt.New(authjwt.Config{
		Key:        authjwt.HMAC(hmacSecret),
		Keyfunc:    kf,
		Algorithms: []string{"HS256"},
	})
	if err == nil {
		t.Fatal("New: got nil error for a Config setting both Key and Keyfunc, want an error")
	}
}

func TestNewRejectsAlgorithmsWithKey(t *testing.T) {
	_, err := authjwt.New(authjwt.Config{
		Key:        authjwt.HMAC(hmacSecret),
		Algorithms: []string{"HS256"},
	})
	if err == nil {
		t.Fatal("New: got nil error for a Config setting Algorithms alongside Key, want an error")
	}
}

func TestValidRSATokenAuthenticates(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.RSAPublicKey(&priv.PublicKey)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	id, err := auth.Authenticate(bearerRequest(t, token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", id.Subject, "user-123")
	}
}

func TestValidECDSATokenAuthenticates(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.ECDSAPublicKey(&priv.PublicKey)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	token, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	id, err := auth.Authenticate(bearerRequest(t, token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", id.Subject, "user-123")
	}
}

func TestValidEdDSATokenAuthenticates(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.EdDSAPublicKey(pub)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	id, err := auth.Authenticate(bearerRequest(t, token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", id.Subject, "user-123")
	}
}

// TestEdDSATokenRejectedByRSAKey exercises the general alg-confusion
// protection for a second key family: an EdDSA token must not verify
// against an RSA-configured authenticator either.
func TestEdDSATokenRejectedByRSAKey(t *testing.T) {
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	auth, err := authjwt.New(authjwt.Config{Key: authjwt.RSAPublicKey(&rsaPriv.PublicKey)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(edPriv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
}

// TestClaimMappingHandlesUnusualShapesSafely proves the claim-to-Attrs
// mapping never panics on an unexpected claim value shape (a number, a JSON
// object) and never invents a value: a plain string claim becomes a single
// value, a listed-but-absent claim is simply omitted (not an empty string),
// and a numeric or object claim value is skipped rather than crashing the
// authenticator on attacker-influenced token content.
func TestClaimMappingHandlesUnusualShapesSafely(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{
		Key:    authjwt.HMAC(hmacSecret),
		Claims: []string{"plainstr", "missing", "num", "obj", "groups"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	claims := baseClaims()
	claims["plainstr"] = "hello"
	claims["num"] = 42
	claims["obj"] = map[string]any{"nested": "value"}
	claims["groups"] = []string{"admin"}
	token := signHMAC(t, claims)

	id, err := auth.Authenticate(bearerRequest(t, token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got, want := id.Attrs["plainstr"], []string{"hello"}; !equalStrings(got, want) {
		t.Errorf("Attrs[plainstr] = %v, want %v", got, want)
	}
	if _, present := id.Attrs["missing"]; present {
		t.Errorf("Attrs[missing] present = %v, want absent for a listed-but-unset claim", id.Attrs["missing"])
	}
	if _, present := id.Attrs["num"]; present {
		t.Errorf("Attrs[num] present = %v, want absent for a numeric claim value", id.Attrs["num"])
	}
	if _, present := id.Attrs["obj"]; present {
		t.Errorf("Attrs[obj] present = %v, want absent for an object claim value", id.Attrs["obj"])
	}
	if got, want := id.Attrs["groups"], []string{"admin"}; !equalStrings(got, want) {
		t.Errorf("Attrs[groups] = %v, want %v", got, want)
	}
}

// TestErrorDoesNotLeakToken proves a rejection error never embeds the raw
// token or the signing key: an authenticator that logs its errors must not
// spill either into a log line.
func TestErrorDoesNotLeakToken(t *testing.T) {
	auth, err := authjwt.New(authjwt.Config{Key: authjwt.HMAC(hmacSecret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A token signed with a different secret fails signature verification.
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims()).
		SignedString([]byte("a-totally-different-secret-not-the-configured-one"))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = auth.Authenticate(bearerRequest(t, token))
	assertUnauthenticated(t, err)
	if msg := err.Error(); strings.Contains(msg, token) || strings.Contains(msg, string(hmacSecret)) {
		t.Errorf("error message leaks the token or key: %q", msg)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
