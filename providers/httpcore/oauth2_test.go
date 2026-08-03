package httpcore

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
)

// clientSecretMarker is the client secret used by every test in this file, and
// the needle the leak assertions search for. It is deliberately long and
// distinctive: a short secret like "sec" can match incidentally in unrelated
// text, and would not catch a leak that truncated the value.
const clientSecretMarker = "cs3cr3t-oauth-marker-4d1b"

// tokenServer returns a RoundTripper answering the token endpoint with a token
// valid for expiresIn seconds, counting how many exchanges it performed.
func tokenServer(t *testing.T, expiresIn int, exchanges *int) roundTripFunc {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/token" {
			resp, _ := newResponse(http.StatusOK, []byte("resource"), nil)
			return resp, nil
		}
		*exchanges++
		if err := req.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if got := req.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q", got)
		}
		body, _ := json.Marshal(map[string]any{
			"access_token": "at-" + req.PostForm.Get("client_id"),
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
		resp, _ := newResponse(http.StatusOK, body, nil)
		return resp, nil
	}
}

func TestOAuth2ClientCredentialsSetsBearer(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	req := newTestRequest(t)
	if err := auth.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-cid" {
		t.Fatalf("Authorization = %q", got)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges)
	}
}

func TestOAuth2CachesToken(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	for range 5 {
		if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 (token should be cached)", exchanges)
	}
}

func TestOAuth2RefreshesBeforeExpiry(t *testing.T) {
	exchanges := 0
	now := time.Unix(1_700_000_000, 0)
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		Leeway:       30 * time.Second,
		HTTPClient:   fakeClient(tokenServer(t, 60, &exchanges)),
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges)
	}

	// 40s later the token has 20s left, inside the 30s leeway, so it refreshes.
	now = now.Add(40 * time.Second)
	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2 (leeway should force a refresh)", exchanges)
	}
}

func TestOAuth2ClassifiesTokenFailure(t *testing.T) {
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusUnauthorized, []byte(`{"error":"invalid_client"}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	err = auth.Apply(context.Background(), newTestRequest(t))
	if !errors.Is(err, mamori.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	// The client secret must never reach an error message.
	if strings.Contains(err.Error(), clientSecretMarker) {
		t.Fatalf("client secret leaked into %q", err.Error())
	}
}

// TestOAuth2FetchesLazily pins that constructing the authenticator performs no
// token exchange.
//
// Building a provider must never block on the network, and a misconfigured
// identity provider must surface as a classified resolve error rather than as a
// constructor failure at process start. Without this test, moving the exchange
// into OAuth2ClientCredentials would pass every other test in this file.
func TestOAuth2FetchesLazily(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	if exchanges != 0 {
		t.Fatalf("exchanges = %d before the first Apply, want 0: construction must not touch the network", exchanges)
	}
	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d after the first Apply, want 1", exchanges)
	}
}

func TestOAuth2RejectsMissingFields(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  OAuth2Config
	}{
		{"no token url", OAuth2Config{ClientID: "c", ClientSecret: "s"}},
		{"no client id", OAuth2Config{TokenURL: "https://idp.test/token", ClientSecret: "s"}},
		{"no client secret", OAuth2Config{TokenURL: "https://idp.test/token", ClientID: "c"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := OAuth2ClientCredentials(tt.cfg); err == nil {
				t.Fatal("OAuth2ClientCredentials returned nil error")
			}
		})
	}
}

// TestOAuth2WaiterReleasedByOwnContext pins that a caller blocked on someone
// else's in-flight exchange leaves when its OWN context expires.
//
// A plain mutex held across the network call would serialize correctly but
// ignore the waiter's ctx entirely, and mamori's reconciler is single-goroutine:
// one Apply wedged behind a hung identity provider would stall reconciliation
// for every field. The test hangs the token endpoint until released, so the
// waiter can only return by honouring its own deadline.
func TestOAuth2WaiterReleasedByOwnContext(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			resp, _ := newResponse(http.StatusOK,
				[]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	// Owner: starts the exchange and blocks in the transport.
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_ = auth.Apply(context.Background(), newTestRequest(t))
	}()
	<-entered

	// Waiter: arrives while the exchange is in flight, with a context that dies.
	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() { waiterErr <- auth.Apply(ctx, newTestRequest(t)) }()
	cancel()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return after its own context was cancelled; it is blocked on the exchange")
	}

	close(release)
	<-ownerDone
}

// TestOAuth2DoesNotExposeSecretByReflection pins that the client secret is not
// reachable through fmt's reflection walk.
//
// The four authenticators in auth.go are immune because they capture their
// credentials in closures. This one holds state, so it must reach the same
// result deliberately: a secret in a struct field would print in cleartext from
// any %+v debug dump or panic trace, and fmt cannot call a String method on a
// value reached through an unexported field, so a redaction method would not
// save it.
func TestOAuth2DoesNotExposeSecretByReflection(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if dump := fmt.Sprintf(verb, auth); strings.Contains(dump, clientSecretMarker) {
			t.Fatalf("client secret reachable via %s: %q", verb, dump)
		}
	}
}

func TestOAuth2ConcurrentApply(t *testing.T) {
	exchanges := 0
	var mu sync.Mutex
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			return tokenServer(t, 3600, &exchanges)(req)
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
				t.Errorf("Apply: %v", err)
			}
		}()
	}
	wg.Wait()
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 under concurrency", exchanges)
	}
}
