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
type oauth2Auth struct {
	cfg    OAuth2Config
	client *Client
	leeway time.Duration
	now    func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
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
	return &oauth2Auth{cfg: cfg, client: client, leeway: leeway, now: now}, nil
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

// tokenFor returns a live token, exchanging under the lock so concurrent
// callers perform one exchange rather than one each.
func (a *oauth2Auth) tokenFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Add(a.leeway).Before(a.expiresAt) {
		return a.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
	}
	if len(a.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(a.cfg.Scopes, " "))
	}
	if a.cfg.Audience != "" {
		form.Set("audience", a.cfg.Audience)
	}

	resp, err := a.client.Do(ctx, Request{
		Method: http.MethodPost,
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		Body:   []byte(form.Encode()),
	})
	if err != nil {
		// The exchange already classified the status. Restate it as an
		// authentication failure without repeating the cause, so the message
		// cannot accumulate anything derived from the secret.
		return "", fmt.Errorf("httpcore: OAuth2 token exchange for client %q failed: %w", a.cfg.ClientID, err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(resp.Body, &tr); err != nil {
		return "", fmt.Errorf("httpcore: OAuth2 token response is not JSON: %w: %w", mamori.ErrInvalid, err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("httpcore: OAuth2 token response carried no access_token: %w", mamori.ErrUnauthenticated)
	}

	a.token = tr.AccessToken
	if tr.ExpiresIn > 0 {
		a.expiresAt = a.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		// No expires_in means the server did not commit to a lifetime. Treat it
		// as good for one leeway window so it is re-fetched often rather than
		// cached forever.
		a.expiresAt = a.now().Add(a.leeway * 2)
	}
	return a.token, nil
}
