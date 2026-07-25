package mamori

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xavidop/mamori/secret"
)

// --- BasicAuth ---

func TestBasicAuthAcceptsRightCredentials(t *testing.T) {
	a := BasicAuth("alice", secret.NewString("correct-horse-battery"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("alice", "correct-horse-battery")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if id.Subject != "alice" {
		t.Fatalf("Identity.Subject = %q, want alice", id.Subject)
	}
}

func TestBasicAuthRejectsWrongPasswordSameLength(t *testing.T) {
	// Same length as the right password so the failure exercises the
	// constant-time comparison itself, not a length-based short-circuit.
	right := "correct-horse-battery"
	wrong := "korrect-horse-battery"
	if len(right) != len(wrong) {
		t.Fatalf("test setup: right and wrong must be the same length, got %d vs %d", len(right), len(wrong))
	}
	a := BasicAuth("alice", secret.NewString(right))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("alice", wrong)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with a wrong password of the same length, want error")
	}
}

func TestBasicAuthRejectsWrongUsername(t *testing.T) {
	a := BasicAuth("alice", secret.NewString("correct-horse-battery"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("mallory", "correct-horse-battery")

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with the wrong username, want error")
	}
}

func TestBasicAuthRejectsMissingHeader(t *testing.T) {
	a := BasicAuth("alice", secret.NewString("correct-horse-battery"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with no Authorization header, want error")
	}
}

func TestBasicAuthErrorNeverIncludesCredential(t *testing.T) {
	a := BasicAuth("alice", secret.NewString("correct-horse-battery"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("mallory", "totally-wrong-secret-value")

	_, err := a.Authenticate(req)
	if err == nil {
		t.Fatal("Authenticate succeeded, want error")
	}
	if got := err.Error(); containsAny(got, "mallory", "totally-wrong-secret-value") {
		t.Fatalf("error message %q must not contain the presented credential", got)
	}
}

func TestBasicAuthChallenge(t *testing.T) {
	a := BasicAuth("alice", secret.NewString("s"))
	c, ok := a.(Challenger)
	if !ok {
		t.Fatal("BasicAuth does not implement Challenger")
	}
	if got, want := c.Challenge(), `Basic realm="mamori"`; got != want {
		t.Fatalf("Challenge() = %q, want %q", got, want)
	}
}

// --- BearerToken ---

func TestBearerTokenAcceptsRightToken(t *testing.T) {
	a := BearerToken(secret.NewString("s3cr3t-t0ken"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t-t0ken")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if id.Subject != "bearer" {
		t.Fatalf("Identity.Subject = %q, want bearer", id.Subject)
	}
}

func TestBearerTokenRejectsWrongTokenSameLength(t *testing.T) {
	right := "s3cr3t-t0ken"
	wrong := "z3cr3t-t0ken"
	if len(right) != len(wrong) {
		t.Fatalf("test setup: right and wrong must be the same length, got %d vs %d", len(right), len(wrong))
	}
	a := BearerToken(secret.NewString(right))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with a wrong token of the same length, want error")
	}
}

func TestBearerTokenRejectsMissingHeader(t *testing.T) {
	a := BearerToken(secret.NewString("s3cr3t-t0ken"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with no Authorization header, want error")
	}
}

func TestBearerTokenRejectsMalformedHeader(t *testing.T) {
	a := BearerToken(secret.NewString("s3cr3t-t0ken"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic s3cr3t-t0ken")

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with a non-Bearer Authorization header, want error")
	}
}

func TestBearerTokenChallenge(t *testing.T) {
	a := BearerToken(secret.NewString("s"))
	c, ok := a.(Challenger)
	if !ok {
		t.Fatal("BearerToken does not implement Challenger")
	}
	if got, want := c.Challenge(), "Bearer"; got != want {
		t.Fatalf("Challenge() = %q, want %q", got, want)
	}
}

// --- APIKey ---

func TestAPIKeyAcceptsRightKey(t *testing.T) {
	a := APIKey("X-API-Key", secret.NewString("api-key-value"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "api-key-value")

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if id.Subject != "apikey" {
		t.Fatalf("Identity.Subject = %q, want apikey", id.Subject)
	}
}

func TestAPIKeyRejectsWrongKeySameLength(t *testing.T) {
	right := "api-key-value"
	wrong := "bpi-key-value"
	if len(right) != len(wrong) {
		t.Fatalf("test setup: right and wrong must be the same length, got %d vs %d", len(right), len(wrong))
	}
	a := APIKey("X-API-Key", secret.NewString(right))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", wrong)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with a wrong key of the same length, want error")
	}
}

func TestAPIKeyRejectsMissingHeader(t *testing.T) {
	a := APIKey("X-API-Key", secret.NewString("api-key-value"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded with no X-API-Key header, want error")
	}
}

func TestAPIKeyUsesNamedHeader(t *testing.T) {
	a := APIKey("X-Custom-Key", secret.NewString("api-key-value"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "api-key-value") // wrong header name

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("Authenticate succeeded reading a header other than the configured one, want error")
	}
}

func TestAPIKeyIsNotAChallenger(t *testing.T) {
	a := APIKey("X-API-Key", secret.NewString("s"))
	if _, ok := a.(Challenger); ok {
		t.Fatal("APIKey must not implement Challenger (bare 401 only)")
	}
}

// --- Fail closed ---

func TestFailClosedBasicAuthFunc(t *testing.T) {
	a := BasicAuthFunc(func() (string, secret.String) { return "", secret.String{} })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("", "") // the "right" empty credential

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("BasicAuthFunc with a zero password allowed an empty-credential request, want deny")
	}
}

func TestFailClosedBearerTokenFunc(t *testing.T) {
	a := BearerTokenFunc(func() secret.String { return secret.String{} })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ") // the "right" empty credential

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("BearerTokenFunc with a zero token allowed an empty-credential request, want deny")
	}
}

func TestFailClosedAPIKeyFunc(t *testing.T) {
	a := APIKeyFunc("X-API-Key", func() secret.String { return secret.String{} })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "") // the "right" empty credential; Header.Set("", "") is a no-op

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("APIKeyFunc with a zero key allowed an empty-credential request, want deny")
	}
}

func TestFailClosedDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Authenticate panicked on a zero credential: %v", r)
		}
	}()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, a := range []Authenticator{
		BasicAuthFunc(func() (string, secret.String) { return "", secret.String{} }),
		BearerTokenFunc(func() secret.String { return secret.String{} }),
		APIKeyFunc("X-API-Key", func() secret.String { return secret.String{} }),
	} {
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("zero-credential Func variant allowed a bare request, want deny")
		}
	}
}

// --- MTLS ---

func TestMTLSDeniesNonTLSRequest(t *testing.T) {
	a := MTLS(MTLSOptions{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("MTLS allowed a non-TLS request, want deny")
	}
}

func TestMTLSDeniesTLSWithNoPeerCert(t *testing.T) {
	a := MTLS(MTLSOptions{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{}

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("MTLS allowed a TLS request with no peer certificate, want deny")
	}
}

func TestMTLSAcceptsAnyVerifiedCertWhenAllowlistsEmpty(t *testing.T) {
	a := MTLS(MTLSOptions{})
	req := requestWithPeerCert("svc-a", nil)

	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if id.Subject != "svc-a" {
		t.Fatalf("Identity.Subject = %q, want svc-a", id.Subject)
	}
}

func TestMTLSAcceptsAllowedCN(t *testing.T) {
	a := MTLS(MTLSOptions{AllowedCNs: []string{"svc-a", "svc-b"}})
	req := requestWithPeerCert("svc-b", nil)

	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
}

func TestMTLSRejectsDisallowedCN(t *testing.T) {
	a := MTLS(MTLSOptions{AllowedCNs: []string{"svc-a"}})
	req := requestWithPeerCert("svc-mallory", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("MTLS allowed a certificate CN not in AllowedCNs, want deny")
	}
}

func TestMTLSAcceptsAllowedDNSName(t *testing.T) {
	a := MTLS(MTLSOptions{AllowedDNSNames: []string{"svc-a.internal"}})
	req := requestWithPeerCert("irrelevant-cn", []string{"svc-a.internal"})

	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
}

func TestMTLSRejectsDisallowedDNSName(t *testing.T) {
	a := MTLS(MTLSOptions{AllowedDNSNames: []string{"svc-a.internal"}})
	req := requestWithPeerCert("irrelevant-cn", []string{"svc-mallory.internal"})

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("MTLS allowed a certificate DNS SAN not in AllowedDNSNames, want deny")
	}
}

func TestMTLSIsNotAChallenger(t *testing.T) {
	a := MTLS(MTLSOptions{})
	if _, ok := a.(Challenger); ok {
		t.Fatal("MTLS must not implement Challenger (bare 401 only)")
	}
}

func requestWithPeerCert(cn string, dnsNames []string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}, DNSNames: dnsNames},
		},
	}
	return req
}

// --- AnyOf / AllOf ---

// countingAuth wraps a fixed result and counts how many times it was asked to
// authenticate, so tests can prove AnyOf evaluates every member regardless of
// where in the list a success or failure occurs.
type countingAuth struct {
	id    Identity
	err   error
	calls *atomic.Int32
}

func (c countingAuth) Authenticate(*http.Request) (Identity, error) {
	c.calls.Add(1)
	return c.id, c.err
}

var errCountingAuthDenied = errors.New("mamori: denied for test")

func TestAnyOfAllowsWhenEitherAllows(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	deny := AuthFunc(func(*http.Request) (Identity, error) { return Identity{}, errCountingAuthDenied })
	allow := AuthFunc(func(*http.Request) (Identity, error) { return Identity{Subject: "ok"}, nil })

	if _, err := AnyOf(deny, allow).Authenticate(req); err != nil {
		t.Fatalf("AnyOf(deny, allow) returned error: %v", err)
	}
	if _, err := AnyOf(allow, deny).Authenticate(req); err != nil {
		t.Fatalf("AnyOf(allow, deny) returned error: %v", err)
	}
	if _, err := AnyOf(deny, deny).Authenticate(req); err == nil {
		t.Fatal("AnyOf(deny, deny) allowed the request, want deny")
	}
}

func TestAnyOfEvaluatesEveryMember(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	// Case 1: the first member already succeeds; the second must still run.
	var calls1, calls2 atomic.Int32
	a := countingAuth{id: Identity{Subject: "first"}, err: nil, calls: &calls1}
	b := countingAuth{id: Identity{}, err: errCountingAuthDenied, calls: &calls2}
	id, err := AnyOf(a, b).Authenticate(req)
	if err != nil {
		t.Fatalf("AnyOf returned error: %v", err)
	}
	if id.Subject != "first" {
		t.Fatalf("Identity.Subject = %q, want first (the first successful member)", id.Subject)
	}
	if calls1.Load() != 1 || calls2.Load() != 1 {
		t.Fatalf("member call counts = (%d, %d), want (1, 1): AnyOf must evaluate every member", calls1.Load(), calls2.Load())
	}

	// Case 2: both fail; both must still have been called exactly once.
	var calls3, calls4 atomic.Int32
	c := countingAuth{id: Identity{}, err: errCountingAuthDenied, calls: &calls3}
	d := countingAuth{id: Identity{}, err: errCountingAuthDenied, calls: &calls4}
	if _, err := AnyOf(c, d).Authenticate(req); err == nil {
		t.Fatal("AnyOf(deny, deny) allowed the request, want deny")
	}
	if calls3.Load() != 1 || calls4.Load() != 1 {
		t.Fatalf("member call counts = (%d, %d), want (1, 1)", calls3.Load(), calls4.Load())
	}
}

func TestAnyOfChallengeIsFirstMemberThatHasOne(t *testing.T) {
	basic := BasicAuth("alice", secret.NewString("s"))
	apikey := APIKey("X-API-Key", secret.NewString("k")) // not a Challenger

	c, ok := AnyOf(basic, apikey).(Challenger)
	if !ok {
		t.Fatal("AnyOf(basic, apikey) does not implement Challenger, want it to via basic")
	}
	if got, want := c.Challenge(), `Basic realm="mamori"`; got != want {
		t.Fatalf("Challenge() = %q, want %q", got, want)
	}

	if _, ok := AnyOf(apikey).(Challenger); ok {
		t.Fatal("AnyOf(apikey) implements Challenger, want bare 401 since no member does")
	}
}

func TestAllOfAllowsOnlyWhenBothAllow(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	deny := AuthFunc(func(*http.Request) (Identity, error) { return Identity{}, errCountingAuthDenied })
	allow := AuthFunc(func(*http.Request) (Identity, error) { return Identity{Subject: "ok"}, nil })

	if _, err := AllOf(allow, allow).Authenticate(req); err != nil {
		t.Fatalf("AllOf(allow, allow) returned error: %v", err)
	}
	if _, err := AllOf(deny, allow).Authenticate(req); err == nil {
		t.Fatal("AllOf(deny, allow) allowed the request, want deny")
	}
	if _, err := AllOf(allow, deny).Authenticate(req); err == nil {
		t.Fatal("AllOf(allow, deny) allowed the request, want deny")
	}
}

// containsAny reports whether s contains any of substrs.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
