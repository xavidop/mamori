package nacos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// loginPath is Nacos's username/password login endpoint, relative to the
// servlet context path.
const loginPath = "v1/auth/login"

// accessTokenParam is the query parameter Nacos's Open API documents for
// carrying the token: "accessToken=${accessToken} should be appended at the end
// of request url".
const accessTokenParam = "accessToken"

// tokenRefreshMargin is how long before a token's stated expiry it is treated as
// expired. Nacos issues an 18000s (five hour) token by default, so a minute of
// margin costs nothing and covers clock skew between this process and the
// server plus the flight time of a request that is authorized on departure and
// checked on arrival.
const tokenRefreshMargin = time.Minute

// loginTimeout bounds one login exchange when the caller's context has no
// deadline of its own. A watch's context has none by construction, so without
// this a hung Nacos server would park the login forever.
const loginTimeout = 15 * time.Second

// tokenAuth is an httpcore.Authenticator that logs into Nacos with a username
// and password and appends the resulting accessToken to every request.
//
// The token goes in the query string, not a header, because that is what the
// Nacos Open API documents. That placement is worse than a header - a proxy
// access log or the server's own request log records the request line in
// plaintext - and it is called out in the module README so an operator can
// decide about it rather than discover it. httpcore already does the part this
// package can do about it: Client.Do redacts the query from every error it
// returns, including the *url.Error net/http wraps a transport failure in, so
// the token never reaches a mamori error, log line, or Report.
//
// Neither the password nor the token is held in a readable field. Both live
// inside closures, for the same reason httpcore's OAuth2ClientCredentials keeps
// its client secret in one: fmt's %+v and %#v walk unexported fields by
// reflection, and reflection cannot call a String method on what it reaches that
// way, so a redaction method would not protect either value from a debug dump or
// a panic trace. A closure renders as an opaque function pointer.
type tokenAuth struct {
	// login performs one exchange and returns the token and its expiry. It
	// closes over the encoded credentials.
	login func(ctx context.Context) (token string, expiry time.Time, err error)

	mu sync.Mutex
	// token returns the cached access token. It is nil until the first
	// successful exchange, and is replaced wholesale by each one.
	token  func() string
	expiry time.Time
}

// newTokenAuth builds a tokenAuth for base (the server address joined with the
// context path). It validates its inputs at construction so a missing credential
// is a startup failure rather than a 403 on every resolve.
func newTokenAuth(base, username, password string, hc *http.Client) (*tokenAuth, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("nacos: username and password are both required: %w", mamori.ErrInvalid)
	}

	// The login client is separate from the provider's resolve client and has
	// no Authenticator of its own, which is what keeps the two from recursing:
	// an authenticated request that needed a token would otherwise ask this
	// authenticator, which would issue a login request, which would ask again.
	lc, err := httpcore.New(httpcore.Config{
		BaseURL:    base,
		HTTPClient: hc,
		UserAgent:  "mamori-nacos",
	})
	if err != nil {
		return nil, fmt.Errorf("nacos: login endpoint: %w", err)
	}

	// Encoded once, here, so the password is never stored anywhere reflection
	// can walk to it.
	body := []byte(url.Values{"username": {username}, "password": {password}}.Encode())

	return &tokenAuth{
		login: func(ctx context.Context) (string, time.Time, error) {
			resp, err := lc.Do(ctx, httpcore.Request{
				Method: http.MethodPost,
				Path:   loginPath,
				Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
				Body:   body,
			})
			if err != nil {
				return "", time.Time{}, fmt.Errorf("nacos: login: %w", err)
			}
			var out struct {
				AccessToken string `json:"accessToken"`
				TokenTTL    int64  `json:"tokenTtl"`
			}
			if err := json.Unmarshal(resp.Body, &out); err != nil {
				// The body is not echoed. A login response is a credential by
				// definition, and an unparseable one is no less so.
				return "", time.Time{}, fmt.Errorf("nacos: login response is not JSON: %w: %w", mamori.ErrUnavailable, err)
			}
			if out.AccessToken == "" {
				return "", time.Time{}, fmt.Errorf("nacos: login returned no accessToken: %w", mamori.ErrUnauthenticated)
			}
			ttl := time.Duration(out.TokenTTL) * time.Second
			if ttl <= 0 {
				// A server that states no TTL gets one exchange per request
				// rather than a token cached forever. Caching a token of
				// unknown lifetime is how a watch keeps sending an expired
				// credential and reports the backend as broken.
				return out.AccessToken, time.Time{}, nil
			}
			return out.AccessToken, time.Now().Add(ttl), nil
		},
	}, nil
}

// Apply appends the access token to req's query, obtaining or refreshing it
// first when needed.
//
// The lock is never held across the login exchange. mamori's reconciler resolves
// on a single goroutine, so an Apply parked behind a hung Nacos server would
// stall reconciliation for every field, not only the one being resolved. A
// concurrent caller that arrives while a refresh is in flight simply performs
// its own rather than queueing behind it: a login is idempotent and cheap, and
// the alternative (single-flight coordination) buys less than the deadlock
// surface it adds here.
func (t *tokenAuth) Apply(ctx context.Context, req *http.Request) error {
	tok, ok := t.cached()
	if !ok {
		lctx := ctx
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			lctx, cancel = context.WithTimeout(ctx, loginTimeout)
			defer cancel()
		}
		fresh, expiry, err := t.login(lctx)
		if err != nil {
			return err
		}
		t.store(fresh, expiry)
		tok = fresh
	}

	q := req.URL.Query()
	q.Set(accessTokenParam, tok)
	req.URL.RawQuery = q.Encode()
	return nil
}

// cached returns the token when one is held and not within tokenRefreshMargin of
// expiry.
func (t *tokenAuth) cached() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == nil {
		return "", false
	}
	if t.expiry.IsZero() || time.Now().Add(tokenRefreshMargin).After(t.expiry) {
		return "", false
	}
	return t.token(), true
}

// store caches tok until expiry. A zero expiry is stored as zero, which cached
// reads as "always stale", so a server that states no TTL is re-authenticated
// each time rather than trusted forever.
func (t *tokenAuth) store(tok string, expiry time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = func() string { return tok }
	t.expiry = expiry
}

// String keeps a tokenAuth out of a formatted message. It is a courtesy, not the
// protection: the credentials are in closures precisely because reflection-based
// verbs ignore this method.
func (t *tokenAuth) String() string { return "nacos.tokenAuth{redacted}" }
