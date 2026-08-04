package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// defaultLeeway is how far before its stated expiry a cached access token is
// treated as expired, so a request is never sent with a token that dies in
// flight. It matches httpcore.DefaultOAuth2Leeway; the two are separate
// constants only because this package does not use httpcore's authenticator.
const defaultLeeway = 30 * time.Second

// loginRequest is the Universal Auth login body.
//
// It is NOT the RFC 6749 client-credentials form. Infisical wants a JSON object
// with camelCase clientId/clientSecret at a vendor-specific path, where the
// standard grant wants grant_type/client_id/client_secret form-encoded at a
// token endpoint. That mismatch is the whole reason
// httpcore.OAuth2ClientCredentials cannot serve this provider.
//
// A value of this type exists only as a local variable inside newUniversalAuth,
// long enough to be marshalled once. It is never stored in a long-lived struct,
// which is what keeps the client secret out of reflection's reach.
type loginRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// loginResponse is the subset of Infisical's login reply this needs.
//
// The field names are the second half of the mismatch with RFC 6749:
// accessToken and expiresIn, not access_token and expires_in. accessTokenMaxTTL
// and tokenType are documented too but deliberately not read: the max TTL
// bounds how long the token can be RENEWED for, which this provider never does
// (it logs in again instead), and tokenType is always "Bearer".
type loginResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiresIn   int64  `json:"expiresIn"`
}

// universalAuthConfig constructs a universalAuth.
type universalAuthConfig struct {
	// client performs the login round trip. It must carry no Authenticator of
	// its own, since the login is what produces the credential.
	client *httpcore.Client
	// clientID is the machine identity's client id.
	clientID string
	// clientSecret returns the machine identity's client secret. It is a
	// function rather than a string so the caller can hold it in a closure too.
	clientSecret func() string
	// leeway overrides defaultLeeway.
	leeway time.Duration
	// now overrides the clock, for tests.
	now func() time.Time
}

// universalAuth exchanges a machine identity's client id and secret for a
// short-lived Infisical access token, caches it, and refreshes it before expiry.
//
// No credential is held in a readable struct field, neither the client secret
// nor the access token it buys. fmt's %v, %+v and %#v walk unexported fields by
// reflection, and reflection cannot call a String or GoString method on a value
// reached that way, so a redaction method would not save one: fmt falls back to
// printing the raw contents. The encoded login body (secret included, in body)
// and the cached access token (in cached) therefore live inside closures, which
// reflection renders as bare function pointers.
//
// The access token earns that treatment as much as the secret does. It is a
// live bearer credential for every secret the machine identity can read, until
// it expires.
type universalAuth struct {
	client   *httpcore.Client
	body     func() []byte
	clientID string
	leeway   time.Duration
	now      func() time.Time

	mu sync.Mutex
	// cached returns the access token from the last successful login. It is nil
	// until there has been one, which is how tokenFor tells "no token yet" from
	// "a token that has expired".
	cached    func() string
	expiresAt time.Time
	inflight  *loginFetch
}

// loginFetch is one in-flight login, shared by every caller that arrives while
// it runs. done is closed once token and err are final, so a waiter that reads
// them after receiving from done sees settled values.
//
// token is a closure for the same reason universalAuth.cached is: this struct
// is reachable from universalAuth.inflight for the duration of an exchange, so
// a plain string field would put the access token back within reflection's
// reach exactly while it is being fetched.
type loginFetch struct {
	done  chan struct{}
	token func() string
	err   error
}

// newUniversalAuth validates cfg and returns the authenticator.
//
// The login body is encoded once, here, so the client secret is captured by a
// closure and never reaches a struct field. The login itself is lazy, on the
// first Apply, so building a provider never blocks on the network and a
// mistyped credential surfaces as a classified resolve error rather than as a
// constructor failure the caller has no ref to attribute it to.
func newUniversalAuth(cfg universalAuthConfig) (*universalAuth, error) {
	switch {
	case cfg.client == nil:
		return nil, fmt.Errorf("mamori/infisical: a login client is required: %w", mamori.ErrInvalid)
	case cfg.clientID == "":
		return nil, fmt.Errorf("mamori/infisical: a client id is required: %w", mamori.ErrInvalid)
	case cfg.clientSecret == nil:
		return nil, fmt.Errorf("mamori/infisical: a client secret is required: %w", mamori.ErrInvalid)
	}

	encoded, err := json.Marshal(loginRequest{ClientID: cfg.clientID, ClientSecret: cfg.clientSecret()})
	if err != nil {
		// Unreachable for two strings, but the error must never carry the value
		// that failed to encode.
		return nil, fmt.Errorf("mamori/infisical: encoding the login request: %w", mamori.ErrInvalid)
	}

	leeway := cfg.leeway
	if leeway <= 0 {
		leeway = defaultLeeway
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &universalAuth{
		client:   cfg.client,
		body:     func() []byte { return encoded },
		clientID: cfg.clientID,
		leeway:   leeway,
		now:      now,
	}, nil
}

// Apply sets the Authorization header, logging in again when the cached token
// is missing or within leeway of expiry.
func (a *universalAuth) Apply(ctx context.Context, req *http.Request) error {
	tok, err := a.tokenFor(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// tokenFor returns a live access token. Concurrent callers perform ONE login
// rather than one each, and a caller waiting on someone else's login is
// released by its own context.
//
// The lock is never held across the network call. A plain mutex would serialize
// callers correctly, but sync.Mutex has no context-aware Lock, so a waiter is
// not woken by its own ctx expiring. That matters here: mamori's reconciler
// runs on a single goroutine, so an Apply wedged behind a hung Infisical would
// stall reconciliation for every field, not only the one being resolved.
// Instead the first caller publishes an inflight loginFetch and the rest select
// on it against their own ctx.
func (a *universalAuth) tokenFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.cached != nil && a.now().Add(a.leeway).Before(a.expiresAt) {
		tok := a.cached()
		a.mu.Unlock()
		return tok, nil
	}
	if f := a.inflight; f != nil {
		a.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return "", f.err
			}
			return f.token(), nil
		case <-ctx.Done():
			return "", fmt.Errorf("mamori/infisical: Universal Auth login for client %q: %w: %w",
				a.clientID, mamori.ErrUnavailable, ctx.Err())
		}
	}
	f := &loginFetch{done: make(chan struct{})}
	a.inflight = f
	a.mu.Unlock()

	tok, expires, err := a.login(ctx)

	a.mu.Lock()
	a.inflight = nil
	if err == nil {
		a.cached, a.expiresAt = func() string { return tok }, expires
	}
	a.mu.Unlock()

	// Settle the result before closing, so a waiter reading after <-f.done sees
	// final values rather than racing these writes.
	f.token, f.err = func() string { return tok }, err
	close(f.done)
	return tok, err
}

// login performs one Universal Auth exchange and returns the token with the
// instant it expires. It touches no shared state, so it runs outside the lock.
func (a *universalAuth) login(ctx context.Context) (token string, expiresAt time.Time, err error) {
	resp, err := a.client.Do(ctx, httpcore.Request{
		Method: http.MethodPost,
		Path:   loginPath,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   a.body(),
	})
	if err != nil {
		// Do already classified the status, and that classification must
		// survive: httpcore's authError preserves a kind the Authenticator
		// supplied and only adds ErrUnauthenticated to an unclassified error,
		// so a 503 from the login endpoint stays transient instead of being
		// reported as a terminal credential failure.
		return "", time.Time{}, fmt.Errorf("mamori/infisical: Universal Auth login for client %q failed: %w", a.clientID, err)
	}

	var lr loginResponse
	if err := json.Unmarshal(resp.Body, &lr); err != nil {
		// The decode error is deliberately not wrapped. encoding/json reports a
		// syntax failure by quoting the offending byte, and this body is the
		// reply to a request that carried the client secret, so the cause is
		// dropped rather than risking a fragment of it in an error string.
		return "", time.Time{}, fmt.Errorf("mamori/infisical: Universal Auth login response is not JSON: %w", mamori.ErrInvalid)
	}
	if lr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("mamori/infisical: Universal Auth login for client %q returned no accessToken: %w",
			a.clientID, mamori.ErrUnauthenticated)
	}

	if lr.ExpiresIn > 0 {
		return lr.AccessToken, a.now().Add(time.Duration(lr.ExpiresIn) * time.Second), nil
	}
	// No expiresIn means the server did not commit to a lifetime. Treat the
	// token as good for two leeway windows so it is re-fetched often rather
	// than cached forever.
	return lr.AccessToken, a.now().Add(a.leeway * 2), nil
}
