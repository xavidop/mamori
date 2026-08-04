package bitwarden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// tokenPath is the identity endpoint's OAuth2 token path, joined onto the
// identity URL.
const tokenPath = "connect/token"

// scopeSecrets is the only scope a Secrets Manager machine account may request.
const scopeSecrets = "api.secrets"

// deviceTypeSDK is the Device-Type header Bitwarden's own SDK sends (21, its
// DeviceType::SDK). The identity server does not appear to require it for the
// client-credentials grant, but sending what the vendor client sends is the
// cheaper side of that uncertainty: a server that starts enforcing it would
// otherwise break every resolve at once.
const deviceTypeSDK = "21"

// identityResponse is the subset of the identity endpoint's token response
// this provider needs.
//
// encrypted_payload is what makes this exchange different from an ordinary
// client-credentials grant, and why httpcore.OAuth2ClientCredentials cannot be
// reused here: it parses only RFC 6749's fields and drops the vendor extension
// that carries the organization key. Without that field the bearer token is
// useless, because every value the API returns is ciphertext.
type identityResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	TokenType        string `json:"token_type"`
	EncryptedPayload string `json:"encrypted_payload"`
}

// payloadClaims is the decrypted encrypted_payload: a single base64 field
// holding the 64-byte organization symmetric key.
type payloadClaims struct {
	EncryptionKey string `json:"encryptionKey"`
}

// exchange is one in-flight identity round trip, shared by every caller that
// arrives while it runs. done is closed once the other fields are final, so a
// waiter that reads them after receiving from done sees settled values.
//
// token is a closure for the same reason session.cached is: this struct is
// reachable from session.inflight for the duration of an exchange, so a plain
// string field would put a live bearer credential back within reflection's
// reach exactly while it is being fetched.
type exchange struct {
	done   chan struct{}
	token  func() string
	orgKey symKey
	err    error
}

// session performs the access-token exchange and caches its two products: the
// bearer token that authorizes API calls, and the organization key that
// decrypts what they return.
//
// It implements httpcore.Authenticator, so the API client authenticates itself
// through the same cache that Resolve reads the organization key from, and a
// single exchange serves both.
//
// No credential is held in a readable field. The bearer token lives in the
// cached closure and the organization key's halves live inside symKey's
// closures, because fmt's %v, %+v and %#v walk unexported fields by reflection
// and cannot be stopped by a String method on any of them. This mirrors
// providers/httpcore/oauth2.go, whose comment on oauth2Auth sets out the
// reasoning in full.
type session struct {
	client    *httpcore.Client
	accessTok func() string
	leeway    time.Duration
	now       func() time.Time

	mu sync.Mutex
	// cached returns the bearer token from the last successful exchange. It is
	// nil until there has been one, which is how ensure tells "no token yet"
	// from "a token that has expired".
	cached    func() string
	orgKey    symKey
	expiresAt time.Time
	inflight  *exchange
}

// Apply sets the Authorization header, exchanging for a new token when the
// cached one is missing or within leeway of expiry. It satisfies
// httpcore.Authenticator.
func (s *session) Apply(ctx context.Context, req *http.Request) error {
	tok, _, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// ensure returns a live bearer token and the organization key that pairs with
// it, performing at most one exchange across concurrent callers.
//
// The lock is never held across the network call. A plain mutex would
// serialize callers correctly but sync.Mutex has no context-aware Lock, so a
// waiter would not be released by its own context expiring. That matters here
// for the reason httpcore's tokenFor gives: mamori's reconciler runs on a
// single goroutine, so an exchange wedged behind a hung identity endpoint
// would stall reconciliation for every field rather than one.
//
// The token and the key are returned together, and always from the same
// exchange, because they are only valid together: an organization key from a
// previous exchange paired with a fresh bearer token would decrypt nothing and
// would surface as an integrity failure rather than as the staleness it is.
func (s *session) ensure(ctx context.Context) (string, symKey, error) {
	s.mu.Lock()
	if s.cached != nil && s.now().Add(s.leeway).Before(s.expiresAt) {
		tok, key := s.cached(), s.orgKey
		s.mu.Unlock()
		return tok, key, nil
	}
	if f := s.inflight; f != nil {
		s.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return "", symKey{}, f.err
			}
			return f.token(), f.orgKey, nil
		case <-ctx.Done():
			return "", symKey{}, fmt.Errorf("bitwarden: waiting for the access token exchange: %w: %w",
				mamori.ErrUnavailable, ctx.Err())
		}
	}
	f := &exchange{done: make(chan struct{})}
	s.inflight = f
	s.mu.Unlock()

	tok, key, expires, err := s.exchange(ctx)

	s.mu.Lock()
	s.inflight = nil
	if err == nil {
		// The superseded organization key is deliberately NOT wiped here. A
		// Resolve that started before this refresh may still be decrypting
		// with it, and secret.String.Zero's doc comment sets out exactly this
		// hazard: copies share one backing array, so wiping on rotation is a
		// use after free with extra steps. mamori's own core never zeroes a
		// superseded value for the same reason. What this package does wipe is
		// every intermediate it alone owns: the HKDF outputs, the decrypted
		// identity payload, and the raw key bytes read out of it.
		s.cached, s.orgKey, s.expiresAt = func() string { return tok }, key, expires
	}
	s.mu.Unlock()

	// Settle the result before closing, so a waiter reading after <-f.done
	// sees final values rather than racing these writes.
	f.token, f.orgKey, f.err = func() string { return tok }, key, err
	close(f.done)
	return tok, key, err
}

// exchange performs one identity round trip and returns the bearer token, the
// organization key, and the instant the token expires. It touches no shared
// state, so it runs outside the lock.
//
// The access token is re-read and re-parsed on every exchange rather than once
// at construction. Exchanges are an hour apart, so the cost is irrelevant, and
// it means a rotated BWS_ACCESS_TOKEN is picked up at the next refresh instead
// of requiring a restart.
func (s *session) exchange(ctx context.Context) (token string, key symKey, expiresAt time.Time, err error) {
	tok, err := parseAccessToken(s.accessTok())
	if err != nil {
		return "", symKey{}, time.Time{}, err
	}
	// The token's derived key opens the identity payload and nothing else, so
	// it is wiped as soon as that payload is open.
	defer tok.key.wipe()

	// Encoded into a local rather than a field, so the client secret is never
	// reachable from the session struct.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"scope":         {scopeSecrets},
		"client_id":     {tok.id},
		"client_secret": {tok.clientSecret()},
	}.Encode()

	resp, err := s.client.Do(ctx, httpcore.Request{
		Method: http.MethodPost,
		Path:   tokenPath,
		Header: http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Accept":       {"application/json"},
			"Device-Type":  {deviceTypeSDK},
		},
		Body: []byte(form),
	})
	if err != nil {
		// Do already classified the status. It is restated against the machine
		// account id without repeating the cause, so the message cannot
		// accumulate anything derived from the secret.
		return "", symKey{}, time.Time{}, fmt.Errorf("bitwarden: access token exchange for machine account %s failed: %w", tok.id, err)
	}

	var ir identityResponse
	if err := json.Unmarshal(resp.Body, &ir); err != nil {
		return "", symKey{}, time.Time{}, fmt.Errorf("bitwarden: identity response is not JSON: %w: %w", mamori.ErrInvalid, err)
	}
	if ir.AccessToken == "" {
		return "", symKey{}, time.Time{}, fmt.Errorf("bitwarden: identity response carried no access_token: %w", mamori.ErrUnauthenticated)
	}
	if ir.EncryptedPayload == "" {
		return "", symKey{}, time.Time{}, fmt.Errorf(
			"bitwarden: identity response carried no encrypted_payload, so the organization key is unavailable: %w", mamori.ErrUnauthenticated)
	}

	orgKey, err := unwrapOrgKey(tok.key, ir.EncryptedPayload)
	if err != nil {
		return "", symKey{}, time.Time{}, err
	}

	expiry := s.now().Add(s.leeway * 2)
	if ir.ExpiresIn > 0 {
		expiry = s.now().Add(time.Duration(ir.ExpiresIn) * time.Second)
	}
	return ir.AccessToken, orgKey, expiry, nil
}

// unwrapOrgKey decrypts the identity endpoint's encrypted_payload with the
// access token's derived key and returns the organization symmetric key it
// carries.
//
// The payload is a type 2 EncString whose plaintext is the JSON object
// {"encryptionKey":"<base64 64 bytes>"}. Both the decrypted JSON and the
// decoded key bytes are wiped once the key has been copied into its closures,
// because each is a complete copy of the credential that opens every secret in
// the organization.
func unwrapOrgKey(tokenKey symKey, payload string) (symKey, error) {
	enc, err := parseEncString(payload)
	if err != nil {
		return symKey{}, fmt.Errorf("bitwarden: identity encrypted_payload: %w", err)
	}

	plaintext, err := tokenKey.decrypt(enc)
	if err != nil {
		// A failure here is almost always an access token that does not match
		// the payload the server minted, which is an authentication problem
		// rather than a corrupt-data one.
		return symKey{}, fmt.Errorf("bitwarden: decrypting the identity encrypted_payload failed, so the access token does not match it: %w", err)
	}
	defer clear(plaintext)

	var claims payloadClaims
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		// The plaintext is the organization key; the decoder's error would
		// quote it, so it is not wrapped.
		return symKey{}, fmt.Errorf("bitwarden: decrypted identity payload is not JSON: %w", mamori.ErrInvalid)
	}
	if claims.EncryptionKey == "" {
		return symKey{}, fmt.Errorf("bitwarden: decrypted identity payload carried no encryptionKey: %w", mamori.ErrInvalid)
	}

	raw, err := base64.StdEncoding.DecodeString(claims.EncryptionKey)
	if err != nil {
		return symKey{}, fmt.Errorf("bitwarden: organization key is not valid base64: %w", mamori.ErrInvalid)
	}
	defer clear(raw)

	return newSymKey(raw)
}
