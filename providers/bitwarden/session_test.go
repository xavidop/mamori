package bitwarden

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// withNow overrides the session clock. It lives in the test file because
// nothing outside a test has any business moving time.
func withNow(fn func() time.Time) Option {
	return func(c *config) { c.now = fn }
}

// TestTokenExchangeIsCached asserts that a second resolve reuses the first
// exchange. Without the cache every resolved field would cost an extra
// identity round trip, and mamori polls.
func TestTokenExchangeIsCached(t *testing.T) {
	f := newFake(t)
	id := secretUUID("cached")
	f.set(id, "value")

	p := f.provider()
	for range 3 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id)); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if n := f.exchangeCount(); n != 1 {
		t.Errorf("token exchanges = %d, want 1", n)
	}
}

// TestTokenRefreshesBeforeExpiry asserts the leeway window is honoured: a
// token within leeway of its stated expiry is re-fetched rather than sent on a
// request it would die during.
func TestTokenRefreshesBeforeExpiry(t *testing.T) {
	f := newFake(t)
	id := secretUUID("expiry")
	f.set(id, "value")

	now := time.Unix(1_700_000_000, 0)
	p := New(
		WithAccessToken(fakeAccessToken),
		WithIdentityURL("https://identity.test"),
		WithAPIURL("https://api.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
		WithLeeway(60*time.Second),
		withNow(func() time.Time { return now }),
	)

	ref := mustRef(t, "bitwarden-sm://"+id)
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if n := f.exchangeCount(); n != 1 {
		t.Fatalf("exchanges after the first resolve = %d, want 1", n)
	}

	// The fake states expires_in=3600. Well inside that, still cached.
	now = now.Add(30 * time.Minute)
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if n := f.exchangeCount(); n != 1 {
		t.Errorf("a token 30 minutes into a 60 minute life was refetched: exchanges = %d, want 1", n)
	}

	// Now inside the leeway window, so it must be refreshed.
	now = now.Add(30*time.Minute - 30*time.Second)
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("third Resolve: %v", err)
	}
	if n := f.exchangeCount(); n != 2 {
		t.Errorf("a token inside the leeway window was not refreshed: exchanges = %d, want 2", n)
	}
}

// TestConcurrentResolvesShareOneExchange asserts the single-flight behaviour.
// Twenty goroutines starting cold must produce ONE token exchange, not twenty:
// a thundering herd against the identity endpoint is how a restart turns into
// a rate limit.
func TestConcurrentResolvesShareOneExchange(t *testing.T) {
	f := newFake(t)
	id := secretUUID("concurrent")
	f.set(id, "value")

	p := f.provider()
	ref := mustRef(t, "bitwarden-sm://"+id)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	start := make(chan struct{})
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := p.Resolve(context.Background(), ref); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Resolve: %v", err)
	}

	if n := f.exchangeCount(); n != 1 {
		t.Errorf("token exchanges = %d, want 1; the single-flight path did not hold", n)
	}
}

// TestExchangeRereadsTheAccessToken asserts a rotated token is picked up at
// the next refresh rather than requiring a restart, which is the whole reason
// the token is read per exchange rather than once at construction.
func TestExchangeRereadsTheAccessToken(t *testing.T) {
	f := newFake(t)
	id := secretUUID("rotation")
	f.set(id, "value")

	token := "0.11111111-1111-1111-1111-111111111111.wrong:X8vbvA0bduihIDe/qrzIQQ=="
	now := time.Unix(1_700_000_000, 0)
	p := New(
		WithIdentityURL("https://identity.test"),
		WithAPIURL("https://api.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
		withNow(func() time.Time { return now }),
		func(c *config) { c.accessTok = func() string { return token } },
	)

	ref := mustRef(t, "bitwarden-sm://"+id)
	if _, err := p.Resolve(context.Background(), ref); err == nil {
		t.Fatal("a token the fake does not accept resolved successfully")
	}

	// Rotate to the credentials the backend knows, without rebuilding anything.
	token = fakeAccessToken
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("after rotating the access token: %v", err)
	}
}

// TestMalformedAccessToken is the access-token parser table. Every case is a
// realistic paste error, and each must fail as ErrInvalid without echoing any
// part of the token.
func TestMalformedAccessToken(t *testing.T) {
	cases := []struct{ name, token string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"no colon", "0." + fakeClientID + "." + fakeClientSecret},
		{"two dot parts", "0." + fakeClientID + ":" + fakeTokenKeyB64},
		{"four dot parts", "0." + fakeClientID + ".a.b:" + fakeTokenKeyB64},
		{"wrong version", "1." + fakeClientID + "." + fakeClientSecret + ":" + fakeTokenKeyB64},
		{"id is not a uuid", "0.not-a-uuid." + fakeClientSecret + ":" + fakeTokenKeyB64},
		{"empty client secret", "0." + fakeClientID + ".:" + fakeTokenKeyB64},
		{"key is not base64", "0." + fakeClientID + "." + fakeClientSecret + ":!!!!"},
		{"key is the wrong length", "0." + fakeClientID + "." + fakeClientSecret + ":AAAA"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAccessToken(tc.token)
			if err == nil {
				t.Fatal("a malformed access token parsed successfully")
			}
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			// The client secret must never reach the message.
			if tc.token != "" && strings.Contains(err.Error(), fakeClientSecret) {
				t.Errorf("parse error leaked the client secret: %v", err)
			}
		})
	}
}

// TestValidAccessTokenParses pins the happy path against the canonical token
// shape from Bitwarden's own round-trip test.
func TestValidAccessTokenParses(t *testing.T) {
	tok, err := parseAccessToken(fakeAccessToken)
	if err != nil {
		t.Fatalf("parseAccessToken: %v", err)
	}
	if tok.id != fakeClientID {
		t.Errorf("id = %q, want %q", tok.id, fakeClientID)
	}
	if tok.clientSecret() != fakeClientSecret {
		t.Error("client secret did not round trip")
	}
	if !tok.key.valid() {
		t.Error("no key was derived")
	}
	// Surrounding whitespace is tolerated: an access token pasted into an env
	// file commonly carries a trailing newline.
	if _, err := parseAccessToken("  " + fakeAccessToken + "\n"); err != nil {
		t.Errorf("a token with surrounding whitespace was rejected: %v", err)
	}
}

// TestIdentityPayloadFailures asserts each way the encrypted payload can be
// wrong is refused rather than producing an unusable key.
func TestIdentityPayloadFailures(t *testing.T) {
	id := secretUUID("payload")

	cases := []struct {
		name   string
		setup  func(*fakeBackend)
		wantIs error
	}{
		{
			name:   "payload absent",
			setup:  func(f *fakeBackend) { f.omitPayload = true },
			wantIs: mamori.ErrUnauthenticated,
		},
		{
			name:   "payload is not an EncString",
			setup:  func(f *fakeBackend) { f.payloadOverride = "not-an-encstring" },
			wantIs: mamori.ErrInvalid,
		},
		{
			name:   "payload is an unauthenticated type 0",
			setup:  func(f *fakeBackend) { f.payloadOverride = "0.AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAA==" },
			wantIs: mamori.ErrInvalid,
		},
		{
			name: "payload was sealed under a different key",
			setup: func(f *fakeBackend) {
				other, err := deriveShareableKey([]byte("0123456789abcdef"), "accesstoken", "sm-access-token")
				if err != nil {
					panic(err)
				}
				f.payloadOverride = sealEncString(fakeTB{}, other, []byte(`{"encryptionKey":"AAAA"}`))
			},
			wantIs: mamori.ErrInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.set(id, "value")
			tc.setup(f)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
			if err == nil {
				t.Fatal("a broken identity payload resolved successfully")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want %v", err, tc.wantIs)
			}
		})
	}
}

// TestOrganizationKeyOfWrongLengthIsRefused asserts a payload carrying a key
// that is not 64 bytes fails at construction rather than being padded,
// truncated, or handed to aes.NewCipher.
func TestOrganizationKeyOfWrongLengthIsRefused(t *testing.T) {
	tokenKey, err := deriveShareableKey([]byte("0123456789abcdef"), "accesstoken", "sm-access-token")
	if err != nil {
		t.Fatalf("deriveShareableKey: %v", err)
	}
	// A 32-byte key, which is a real Bitwarden variant for other key types and
	// so the most plausible wrong length to receive here.
	payload := sealEncString(t, tokenKey, []byte(`{"encryptionKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`))

	if _, err := unwrapOrgKey(tokenKey, payload); err == nil {
		t.Fatal("an organization key of the wrong length was accepted")
	}
}

// TestContextCancellationIsHonoured asserts a dead context stops the resolve
// at the first round trip rather than being noticed only afterwards.
func TestContextCancellationIsHonoured(t *testing.T) {
	f := newFake(t)
	id := secretUUID("cancel")
	f.set(id, "value")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.provider().Resolve(ctx, mustRef(t, "bitwarden-sm://"+id)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
