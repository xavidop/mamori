package infisical

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// newTestAuth builds a universalAuth against f, with an optional clock. It
// exists so the tests below assert on the authenticator directly rather than
// through a Resolve, which would mix the read path's behaviour into every
// result.
func newTestAuth(t *testing.T, f *fakeInfisical, now func() time.Time) *universalAuth {
	t.Helper()
	c, err := httpcore.New(httpcore.Config{
		BaseURL:    "https://infisical.test",
		HTTPClient: &http.Client{Transport: f.transport()},
	})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	a, err := newUniversalAuth(universalAuthConfig{
		client:       c,
		clientID:     testClientID,
		clientSecret: func() string { return testClientSecret },
		now:          now,
	})
	if err != nil {
		t.Fatalf("newUniversalAuth: %v", err)
	}
	return a
}

// applyTo runs Apply against a throwaway request and returns the Authorization
// header it set.
func applyTo(t *testing.T, a *universalAuth, ctx context.Context) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://infisical.test/api/v4/secrets/X", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if err := a.Apply(ctx, req); err != nil {
		return "", err
	}
	return req.Header.Get("Authorization"), nil
}

// TestApplySetsBearerToken pins the header the read path depends on.
func TestApplySetsBearerToken(t *testing.T) {
	a := newTestAuth(t, newFake(), nil)

	got, err := applyTo(t, a, context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if want := "Bearer " + testAccessToken; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// TestLoginPostsTheDocumentedJSONBody pins the half of the vendor contract that
// httpcore.OAuth2ClientCredentials could not satisfy: Infisical wants a JSON
// object with camelCase clientId/clientSecret, not the RFC 6749
// form-encoded grant_type/client_id/client_secret.
func TestLoginPostsTheDocumentedJSONBody(t *testing.T) {
	f := newFake()
	a := newTestAuth(t, f, nil)

	if _, err := applyTo(t, a, context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	f.mu.Lock()
	body := append([]byte(nil), f.lastLoginBody...)
	f.mu.Unlock()

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("login body is not JSON: %v", err)
	}
	if got["clientId"] != testClientID {
		t.Errorf("clientId = %v, want %q", got["clientId"], testClientID)
	}
	if got["clientSecret"] != testClientSecret {
		t.Errorf("clientSecret = %v, want the configured secret", got["clientSecret"])
	}
	for _, rfc6749 := range []string{"grant_type", "client_id", "client_secret"} {
		if _, present := got[rfc6749]; present {
			t.Errorf("login body carries the RFC 6749 field %q; Infisical's endpoint does not read it", rfc6749)
		}
	}
}

// TestAuthenticatorNeverPrintsItsCredentials is the reason both the client
// secret and the access token live in closures rather than struct fields.
//
// fmt's %+v and %#v walk unexported fields by reflection, and reflection cannot
// call a String or GoString method on a value it reaches that way, so a
// redaction method would not have protected either one: fmt falls back to
// printing the raw contents. A closure renders as a bare function pointer. The
// access token earns that treatment as much as the secret does, because it is a
// live bearer credential for every secret the identity can read.
func TestAuthenticatorNeverPrintsItsCredentials(t *testing.T) {
	a := newTestAuth(t, newFake(), nil)

	// The pointer is printed rather than the dereferenced value, and that loses
	// nothing: %v, %+v and %#v all follow a struct pointer one level and walk
	// the fields behind it, which is exactly the reflection this guards
	// against. Printing *a instead would additionally copy a sync.Mutex, which
	// `go vet` refuses - so no caller can print the dereferenced form either
	// without CI telling them.
	//
	// Both before AND after a successful exchange: before, only the encoded
	// login body exists; afterwards the cached access token exists too, in a
	// different field.
	assertNoCredentials := func(when string) {
		t.Helper()
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(verb, a)
			if strings.Contains(rendered, testClientSecret) {
				t.Errorf("%s: fmt.Sprintf(%q, authenticator) leaked the client secret: %s", when, verb, rendered)
			}
			if strings.Contains(rendered, testAccessToken) {
				t.Errorf("%s: fmt.Sprintf(%q, authenticator) leaked the access token: %s", when, verb, rendered)
			}
		}
	}

	assertNoCredentials("before the exchange")
	if _, err := applyTo(t, a, context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertNoCredentials("after the exchange")
}

// TestTokenIsCachedUntilLeewayBeforeExpiry pins both halves of the refresh
// rule with a controlled clock: a token inside its lifetime is reused, and one
// within leeway of expiry is replaced rather than sent on a request it would
// die in the middle of.
func TestTokenIsCachedUntilLeewayBeforeExpiry(t *testing.T) {
	f := newFake()
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	a := newTestAuth(t, f, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	if _, err := applyTo(t, a, context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// The fake issues expiresIn=3600 and defaultLeeway is 30s, so well inside
	// the window the cached token must be reused.
	advance(3000 * time.Second)
	if _, err := applyTo(t, a, context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if logins, _ := f.counts(); logins != 1 {
		t.Fatalf("logins = %d, want 1: a live token must be reused", logins)
	}

	// Now inside the leeway window: 3600 - 3000 - 590 = 10s left, under 30s.
	advance(590 * time.Second)
	if _, err := applyTo(t, a, context.Background()); err != nil {
		t.Fatalf("third Apply: %v", err)
	}
	if logins, _ := f.counts(); logins != 2 {
		t.Fatalf("logins = %d, want 2: a token within leeway of expiry must be replaced before it is used", logins)
	}
}

// TestConcurrentApplyPerformsOneLogin pins that callers arriving while an
// exchange is in flight share it instead of each starting their own. Without
// the inflight record, a burst of refs resolving at once would each buy a
// token, which is both wasteful and the fastest way to trip an identity
// provider's rate limit.
func TestConcurrentApplyPerformsOneLogin(t *testing.T) {
	f := newFake()
	a := newTestAuth(t, f, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := applyTo(t, a, context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Apply: %v", err)
	}

	if logins, _ := f.counts(); logins != 1 {
		t.Fatalf("logins = %d, want 1: concurrent callers must share one exchange", logins)
	}
}

// TestWaiterIsReleasedByItsOwnContext pins the reason the lock is never held
// across the network call.
//
// sync.Mutex has no context-aware Lock, so a caller queued behind a hung
// exchange could not be woken by its own deadline. mamori's reconciler runs on
// a single goroutine, so one Apply wedged behind a hung Infisical would stall
// reconciliation for every field, not only the one being resolved. Here the
// first caller's exchange is held open while a second caller, whose context is
// already cancelled, must come back immediately.
func TestWaiterIsReleasedByItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once

	c, err := httpcore.New(httpcore.Config{
		BaseURL: "https://infisical.test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			once.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return jsonResp(http.StatusOK, `{"accessToken":"`+testAccessToken+`","expiresIn":3600}`), nil
		})},
	})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	a, err := newUniversalAuth(universalAuthConfig{
		client:       c,
		clientID:     testClientID,
		clientSecret: func() string { return testClientSecret },
	})
	if err != nil {
		t.Fatalf("newUniversalAuth: %v", err)
	}

	first := make(chan error, 1)
	go func() {
		_, err := applyTo(t, a, context.Background())
		first <- err
	}()
	<-blocked // the exchange is in flight and will not finish on its own

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := applyTo(t, a, cancelled)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want it to wrap context.Canceled", err)
		}
		if !errors.Is(err, mamori.ErrUnavailable) {
			t.Fatalf("waiter err = %v, want ErrUnavailable: a hung identity provider is transient, not a terminal credential failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter with a cancelled context was not released; the lock is being held across the network call")
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
}

// TestLoginFailureIsClassified pins that a status from the login endpoint keeps
// its own kind rather than being flattened.
//
// The 503 case is the load-bearing one. httpcore's authError adds
// ErrUnauthenticated only to an UNCLASSIFIED authenticator error, because
// mamori.ErrorKind tests KindUnauthenticated before KindUnavailable: adding it
// unconditionally would report a passing blip at the identity provider as a
// terminal credential failure, and mamori treats unauthenticated as terminal
// while unavailable is expected to heal.
func TestLoginFailureIsClassified(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"bad credentials", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"identity revoked", http.StatusForbidden, mamori.KindPermissionDenied},
		{"identity provider down", http.StatusServiceUnavailable, mamori.KindUnavailable},
		{"rate limited", http.StatusTooManyRequests, mamori.KindRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(conformanceRef("TOKEN"), "value")
			f.failLogin(tt.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
			if err == nil {
				t.Fatalf("login status %d produced no error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("login status %d kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestLoginErrorNeverCarriesTheSecret pins that a rejected login does not put
// the credential it sent into the error. The login response is the one body in
// this provider that is a reply to a request containing the client secret,
// which is why the login client sets no ErrorDetail hook.
func TestLoginErrorNeverCarriesTheSecret(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failLogin(http.StatusUnauthorized)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testClientSecret) {
		t.Fatalf("the client secret leaked into %q", err)
	}
}

// TestLoginWithoutAccessTokenIsUnauthenticated pins the 200-with-no-token case.
// A backend answering 200 with an envelope this provider cannot read must fail
// loudly, not set "Bearer " with nothing after it and let the read path report
// a confusing 401.
func TestLoginWithoutAccessTokenIsUnauthenticated(t *testing.T) {
	c, err := httpcore.New(httpcore.Config{
		BaseURL: "https://infisical.test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(http.StatusOK, `{"expiresIn":3600,"tokenType":"Bearer"}`), nil
		})},
	})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	a, err := newUniversalAuth(universalAuthConfig{
		client:       c,
		clientID:     testClientID,
		clientSecret: func() string { return testClientSecret },
	})
	if err != nil {
		t.Fatalf("newUniversalAuth: %v", err)
	}

	_, err = applyTo(t, a, context.Background())
	if !errors.Is(err, mamori.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestLoginResponseThatIsNotJSONNeverEchoesTheBody pins the deliberate absence
// of a wrapped cause on the decode path. encoding/json quotes the offending
// byte in a syntax error, and this body is the reply to a request that carried
// the client secret, so the cause is dropped rather than risking a fragment of
// it in an error string.
func TestLoginResponseThatIsNotJSONNeverEchoesTheBody(t *testing.T) {
	const garbage = "not-json-but-maybe-a-credential"
	c, err := httpcore.New(httpcore.Config{
		BaseURL: "https://infisical.test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(http.StatusOK, garbage), nil
		})},
	})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	a, err := newUniversalAuth(universalAuthConfig{
		client:       c,
		clientID:     testClientID,
		clientSecret: func() string { return testClientSecret },
	})
	if err != nil {
		t.Fatalf("newUniversalAuth: %v", err)
	}

	_, err = applyTo(t, a, context.Background())
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), garbage[:8]) {
		t.Fatalf("the login response body reached the error: %q", err)
	}
	// The substring check above cannot fail on its own, and asserting only it
	// would be a test that proves nothing. encoding/json quotes a SINGLE byte in
	// a syntax error, so wrapping the cause yields "invalid character 'o' in
	// literal null", which contains no eight-character run of the body. What
	// actually distinguishes a dropped cause from a wrapped one is whether any
	// encoding/json text reaches the message at all.
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("the json decode cause was wrapped, so a quoted byte of the body can reach the error: %q", err)
	}
}

// TestNewUniversalAuthRejectsMissingInputs pins that the authenticator refuses
// to be built without what it needs, so a wiring mistake surfaces where the
// wiring is rather than as an opaque 401 on every resolve.
func TestNewUniversalAuthRejectsMissingInputs(t *testing.T) {
	c, err := httpcore.New(httpcore.Config{BaseURL: "https://infisical.test"})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	secret := func() string { return testClientSecret }

	tests := []struct {
		name string
		cfg  universalAuthConfig
	}{
		{"no client", universalAuthConfig{clientID: testClientID, clientSecret: secret}},
		{"no client id", universalAuthConfig{client: c, clientSecret: secret}},
		{"no client secret", universalAuthConfig{client: c, clientID: testClientID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newUniversalAuth(tt.cfg); !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}
