package httpcore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// DefaultOAuth2Leeway is how far before its stated expiry a cached token is
// treated as expired, so a request is not sent with a token that dies in flight.
const DefaultOAuth2Leeway = 30 * time.Second

// OAuth2Config configures the client-credentials grant.
type OAuth2Config struct {
	// TokenURL is the token endpoint. Required.
	TokenURL string
	// ClientID is the OAuth2 client identifier. Required.
	ClientID string
	// ClientSecret is the OAuth2 client secret. Required.
	ClientSecret string
	// Scopes is the optional scope list.
	Scopes []string
	// Audience is the optional audience parameter, which several providers
	// require.
	Audience string
	// HTTPClient performs the token exchange. When nil, a client with
	// DefaultTimeout is used.
	HTTPClient *http.Client
	// Leeway overrides DefaultOAuth2Leeway.
	Leeway time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// oauth2Auth caches one access token and refreshes it before expiry.
//
// It deliberately does NOT retain the OAuth2Config. fmt's %v, %+v and %#v walk
// unexported struct fields through reflection, and cannot call a String method
// on a value reached that way, so a ClientSecret held in a field here would
// print in cleartext from any debug dump or panic trace. The four authenticators
// in auth.go are immune because they capture their credentials in closures, and
// this one does the same: the encoded form body, secret included, lives only
// inside the form closure, which reflection renders as a function pointer.
type oauth2Auth struct {
	client   *Client
	form     func() []byte
	clientID string
	leeway   time.Duration
	now      func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	inflight  *tokenFetch
}

// tokenFetch is one in-flight token exchange, shared by every caller that
// arrives while it runs. done is closed once token and err are final, so a
// waiter that reads them after receiving from done sees settled values.
type tokenFetch struct {
	done  chan struct{}
	token string
	err   error
}

// tokenResponse is the subset of RFC 6749 section 5.1 this needs.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// OAuth2ClientCredentials returns an Authenticator that performs the RFC 6749
// client-credentials grant, caches the access token, and refreshes it before it
// expires.
//
// The token is fetched lazily on the first Apply rather than at construction,
// so building a provider never blocks on the network and a misconfigured
// identity provider surfaces as a classified resolve error rather than a
// constructor failure.
//
// The client secret is never logged and never appears in a returned error.
func OAuth2ClientCredentials(cfg OAuth2Config) (Authenticator, error) {
	switch {
	case cfg.TokenURL == "":
		return nil, fmt.Errorf("httpcore: OAuth2 TokenURL is required: %w", mamori.ErrInvalid)
	case cfg.ClientID == "":
		return nil, fmt.Errorf("httpcore: OAuth2 ClientID is required: %w", mamori.ErrInvalid)
	case cfg.ClientSecret == "":
		return nil, fmt.Errorf("httpcore: OAuth2 ClientSecret is required: %w", mamori.ErrInvalid)
	}

	client, err := New(Config{BaseURL: cfg.TokenURL, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("httpcore: OAuth2 TokenURL: %w", err)
	}

	leeway := cfg.Leeway
	if leeway <= 0 {
		leeway = DefaultOAuth2Leeway
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	// Encode the form once, here, so the client secret is captured by the
	// closure below and never stored in a struct field where reflection could
	// reach it. See the note on oauth2Auth.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	if cfg.Audience != "" {
		form.Set("audience", cfg.Audience)
	}
	encoded := form.Encode()

	return &oauth2Auth{
		client:   client,
		form:     func() []byte { return []byte(encoded) },
		clientID: cfg.ClientID,
		leeway:   leeway,
		now:      now,
	}, nil
}

// Apply sets the Authorization header, exchanging for a new token when the
// cached one is missing or within Leeway of expiry.
func (a *oauth2Auth) Apply(ctx context.Context, req *http.Request) error {
	tok, err := a.tokenFor(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// tokenFor returns a live token. Concurrent callers perform ONE exchange rather
// than one each, and a caller waiting on someone else's exchange is released by
// its own context.
//
// The lock is never held across the network call. A plain mutex would serialize
// callers correctly, which is what golang.org/x/oauth2 does, but sync.Mutex has
// no context-aware Lock, so a waiter is not woken by its own ctx expiring. That
// matters here more than it does for a general OAuth2 client: mamori's
// reconciler is single-goroutine, so an Apply wedged behind a hung identity
// provider would stall reconciliation for every field, not just the one being
// resolved. Instead the first caller publishes an inflight tokenFetch and the
// rest select on it against their own ctx.
func (a *oauth2Auth) tokenFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.now().Add(a.leeway).Before(a.expiresAt) {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	if f := a.inflight; f != nil {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.token, f.err
		case <-ctx.Done():
			return "", fmt.Errorf("httpcore: OAuth2 token exchange for client %q: %w: %w",
				a.clientID, mamori.ErrUnavailable, ctx.Err())
		}
	}
	f := &tokenFetch{done: make(chan struct{})}
	a.inflight = f
	a.mu.Unlock()

	tok, expires, err := a.exchange(ctx)

	a.mu.Lock()
	a.inflight = nil
	if err == nil {
		a.token, a.expiresAt = tok, expires
	}
	a.mu.Unlock()

	// Settle the result before closing, so a waiter reading after <-f.done sees
	// final values rather than racing these writes.
	f.token, f.err = tok, err
	close(f.done)
	return tok, err
}

// exchange performs one client-credentials round trip and returns the token with
// the instant it expires. It touches no shared state, so it runs outside the lock.
func (a *oauth2Auth) exchange(ctx context.Context) (token string, expiresAt time.Time, err error) {
	resp, err := a.client.Do(ctx, Request{
		Method: http.MethodPost,
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		Body:   a.form(),
	})
	if err != nil {
		// Do already classified the status. Restate it as an authentication
		// failure without repeating the cause, so the message cannot accumulate
		// anything derived from the secret.
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token exchange for client %q failed: %w", a.clientID, err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(resp.Body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token response is not JSON: %w: %w", mamori.ErrInvalid, err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token response carried no access_token: %w", mamori.ErrUnauthenticated)
	}

	if tr.ExpiresIn > 0 {
		return tr.AccessToken, a.now().Add(time.Duration(tr.ExpiresIn) * time.Second), nil
	}
	// No expires_in means the server did not commit to a lifetime. Treat it as
	// good for two leeway windows so it is re-fetched often rather than cached
	// forever.
	return tr.AccessToken, a.now().Add(a.leeway * 2), nil
}
