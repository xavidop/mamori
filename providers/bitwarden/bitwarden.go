// Package bitwarden is a mamori provider for Bitwarden Secrets Manager, the
// machine-account product behind the `bws` CLI. It is not a provider for the
// consumer password manager, which speaks a different API and a different key
// hierarchy.
//
// # Scheme
//
//	bitwarden-sm://<secret-uuid>[#key][?opts]
//
// A secret is addressed by its UUID, which is what the Bitwarden UI's "Copy
// secret ID" yields and what `bws secret get` takes:
//
//	type Config struct {
//	    StripeKey secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d"`
//	    DBPass    secret.String `source:"bitwarden-sm://6b3f9e0c-9f9a-4a5c-9a09-af9601317f2d#password"`
//	}
//
// The resolved value is the secret's decrypted `value`. A #key fragment is
// handed to mamori.SelectKey, so it selects out of that value when it is a
// JSON document, identically to every other mamori provider.
//
// Addressing is by UUID and not by name, deliberately. Bitwarden's list
// endpoint returns each secret's name as ciphertext and omits its value
// entirely, so a name lookup would mean fetching and decrypting every secret
// in the organization on every resolve, and a name is not unique across
// projects. A UUID is stable, unique, and costs one request.
//
// # Why this provider decrypts locally
//
// Bitwarden is end-to-end encrypted: the API returns ciphertext and the server
// cannot read it. A provider that returned what the API returned would be
// returning base64, not a secret. So this package performs the full client-side
// unwrap, and it does so with the Go standard library alone, as every module in
// this repository must:
//
//  1. The access token `0.<uuid>.<secret>:<base64 16 bytes>` is split. Its
//     16-byte key is stretched with HKDF-SHA256 (crypto/hkdf), salt
//     "bitwarden-accesstoken", info "sm-access-token", to 64 bytes.
//  2. Those bytes decrypt the identity endpoint's `encrypted_payload`, which
//     yields the 64-byte organization symmetric key.
//  3. That key decrypts the secret's `value`.
//
// Steps 2 and 3 are the same primitive: a type 2 EncString, `2.<iv>|<ct>|<mac>`,
// AES-256-CBC with HMAC-SHA256 in Encrypt-then-MAC. The MAC is always verified
// before the ciphertext is decrypted, with crypto/hmac.Equal. See crypto.go.
//
// Bitwarden's own SDK is a Rust core reached through cgo bindings, which this
// repository cannot take as a dependency. The derivation and the cipher here
// are asserted against Bitwarden's own published test vectors; see the module
// README for exactly what that does and does not establish.
//
// # Watching
//
// Secrets Manager exposes no push channel, so this provider does not implement
// mamori.WatchableProvider and mamori wraps it in the polling adapter.
// Value.Version is the secret's revisionDate, a plaintext field the API returns
// beside the ciphertext, so an unchanged secret yields a stable version without
// comparing plaintext.
package bitwarden

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the ref scheme this provider registers. It names the Secrets
// Manager product rather than "bitwarden" alone, matching the aws-sm / gcp-sm /
// scaleway-sm convention already in this repository and leaving room for a
// password-manager provider that would share nothing but the vendor.
const scheme = "bitwarden-sm"

// Default cloud endpoints. Bitwarden's EU cloud and any self-hosted install
// need WithServerURL or the two explicit URL options.
const (
	defaultIdentityURL = "https://identity.bitwarden.com"
	defaultAPIURL      = "https://api.bitwarden.com"
)

// accessTokenEnv is the environment variable `bws` itself reads, so a host
// already configured for the Bitwarden CLI needs no further setup.
const accessTokenEnv = "BWS_ACCESS_TOKEN"

// defaultTimeout bounds a single HTTP round trip when no client is injected.
const defaultTimeout = 30 * time.Second

// Provider resolves bitwarden-sm:// refs against Bitwarden Secrets Manager.
// It is safe for concurrent use.
type Provider struct {
	sess *session
	api  *httpcore.Client
	// err records a construction failure so New can keep the signature that
	// lets init register a provider, while a misconfigured URL still surfaces
	// as a classified error from the first Resolve, which is where
	// `mamori doctor` looks.
	err error
	// httpClient is config.httpClient, held here only so Close can release its
	// idle connections. It is never used to build a request directly; sess and
	// api (each built from it) are.
	httpClient *http.Client

	mu     sync.Mutex
	closed bool
}

// Option configures a Provider.
type Option func(*config)

// config collects option values before New validates them and builds clients.
type config struct {
	accessTok     func() string
	identityURL   string
	apiURL        string
	httpClient    *http.Client
	maxBody       int64
	leeway        time.Duration
	now           func() time.Time
	allowInsecure bool
}

// WithAccessToken supplies the machine account access token explicitly, for
// deployments that hold it somewhere other than the environment.
//
// The value is captured in a closure rather than stored, so it cannot be
// reached by reflection from the Provider; see the comment on session.
func WithAccessToken(token string) Option {
	return func(c *config) { c.accessTok = func() string { return token } }
}

// WithServerURL points the provider at a self-hosted Bitwarden install,
// deriving both endpoints the way `bws` does: base + "/identity" and
// base + "/api". Use it for Bitwarden's EU cloud too, with
// https://vault.bitwarden.eu.
func WithServerURL(base string) Option {
	return func(c *config) {
		base = strings.TrimRight(base, "/")
		if base == "" {
			return
		}
		c.identityURL = base + "/identity"
		c.apiURL = base + "/api"
	}
}

// WithIdentityURL overrides the identity endpoint alone, for an install whose
// identity service is not at the conventional path.
func WithIdentityURL(u string) Option {
	return func(c *config) {
		if u != "" {
			c.identityURL = u
		}
	}
}

// WithAPIURL overrides the API endpoint alone, the counterpart to
// WithIdentityURL.
func WithAPIURL(u string) Option {
	return func(c *config) {
		if u != "" {
			c.apiURL = u
		}
	}
}

// WithHTTPClient injects the client used for both endpoints. It is the seam
// tests use to serve an in-process fake through a custom RoundTripper, and the
// hook for a deployment that needs a proxy or a pinned TLS config.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithMaxBody caps the response size accepted from either endpoint. Zero
// selects httpcore.DefaultMaxBody.
func WithMaxBody(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxBody = n
		}
	}
}

// WithLeeway sets how far before its stated expiry a cached access token is
// treated as expired, so a request is not sent with a token that dies in
// flight. Zero selects httpcore.DefaultOAuth2Leeway.
func WithLeeway(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.leeway = d
		}
	}
}

// WithAllowInsecure permits an http:// endpoint. The token exchange POSTs the
// client secret in its form body and the API returns the organization's
// secrets, so cleartext exposes both to anything on the path; it exists for a
// local test install and must be opted into.
func WithAllowInsecure(yes bool) Option {
	return func(c *config) { c.allowInsecure = yes }
}

// New constructs a Bitwarden Secrets Manager provider. Without options it
// targets Bitwarden's public cloud and reads BWS_ACCESS_TOKEN lazily, at
// resolve time, so registering it from init is safe even when no token is
// present at process start.
//
// It returns a Provider rather than (Provider, error) so that init can
// register one, matching providers/doppler. A bad explicit URL is recorded and
// returned from the first Resolve as mamori.ErrInvalid.
func New(opts ...Option) *Provider {
	c := &config{
		accessTok:   func() string { return os.Getenv(accessTokenEnv) },
		identityURL: defaultIdentityURL,
		apiURL:      defaultAPIURL,
		httpClient:  &http.Client{Timeout: defaultTimeout},
		leeway:      httpcore.DefaultOAuth2Leeway,
		now:         time.Now,
	}
	for _, o := range opts {
		o(c)
	}

	if err := checkScheme("identity URL", c.identityURL, c.allowInsecure); err != nil {
		return &Provider{err: err, httpClient: c.httpClient}
	}
	if err := checkScheme("API URL", c.apiURL, c.allowInsecure); err != nil {
		return &Provider{err: err, httpClient: c.httpClient}
	}

	identity, err := httpcore.New(httpcore.Config{
		BaseURL:    c.identityURL,
		HTTPClient: c.httpClient,
		MaxBody:    c.maxBody,
		UserAgent:  "mamori-bitwarden",
		// The identity endpoint answers a rejected exchange with RFC 6749's
		// error object, whose "error" member is a fixed code such as
		// "invalid_client". That code cannot contain the client secret or any
		// secret value, so it is safe to surface and turns an opaque 400 into
		// a diagnosable one. Sibling members are deliberately not read:
		// "error_description" is free text the server composes, and this
		// package does not assume what a vendor might put in it.
		ErrorDetail: identityErrorDetail,
	})
	if err != nil {
		return &Provider{err: fmt.Errorf("bitwarden: identity URL: %w", err), httpClient: c.httpClient}
	}

	sess := &session{
		client:    identity,
		accessTok: c.accessTok,
		leeway:    c.leeway,
		now:       c.now,
	}

	api, err := httpcore.New(httpcore.Config{
		BaseURL:    c.apiURL,
		HTTPClient: c.httpClient,
		Auth:       sess,
		MaxBody:    c.maxBody,
		UserAgent:  "mamori-bitwarden",
		// No ErrorDetail here, which is the safe default httpcore documents.
		// This endpoint's bodies carry secret material, and no field of its
		// error shape has been established as free of it.
	})
	if err != nil {
		return &Provider{err: fmt.Errorf("bitwarden: API URL: %w", err), httpClient: c.httpClient}
	}

	return &Provider{sess: sess, api: api, httpClient: c.httpClient}
}

// identityErrorDetail extracts RFC 6749's "error" code from a rejected token
// exchange. Anything unparsable yields no detail rather than a guess.
func identityErrorDetail(_ int, body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.Error
}

// checkScheme rejects an endpoint this package will not send credentials to.
//
// The scheme is matched against a closed set rather than only rejecting
// http://, for the reason httpcore's checkTokenURLScheme gives: httpcore.New
// requires only a scheme and a host, so an ftp:// typo passes construction and
// then fails on every resolve with net/http's "unsupported protocol scheme". A
// misconfiguration must fail where `mamori doctor` sees it.
func checkScheme(what, raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		// The unparsed string is not echoed: a URL can carry userinfo.
		return fmt.Errorf("bitwarden: %s is not a URL: %w: %w", what, mamori.ErrInvalid, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf("bitwarden: %s is http://, which exposes the access token and every secret it fetches; set WithAllowInsecure to accept that: %w",
				what, mamori.ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("bitwarden: %s scheme %q is not http or https: %w", what, u.Scheme, mamori.ErrInvalid)
	}
}

// init registers a provider reading BWS_ACCESS_TOKEN, so a blank import is
// enough for the common case.
func init() { mamori.Register(New()) }

// Scheme returns "bitwarden-sm".
func (p *Provider) Scheme() string { return scheme }

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, without contacting Bitwarden.
//
// A client supplied through WithHTTPClient is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the caller's
// own use of that client is unaffected by closing this provider. This is safe
// even on a Provider built from a failed New (p.err set, httpClient still
// recorded): Close never dials, so there is nothing for the earlier failure to
// have left half-done.
//
// CloseIdleConnections is skipped when the tracked client's Transport is nil.
// New's own default (unless overridden by WithHTTPClient) is exactly that
// shape - &http.Client{Timeout: defaultTimeout} with no Transport set - and
// net/http resolves a nil Transport to the process-global
// http.DefaultTransport. Calling CloseIdleConnections on that client would
// evict idle connections belonging to whatever OTHER code in this process
// also leaves its Transport unset (anything built on http.DefaultClient), not
// just this provider's own traffic, so the guard fires on an ordinary,
// never-injected Provider too.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.httpClient != nil && p.httpClient.Transport != nil {
		p.httpClient.CloseIdleConnections()
	}
	return nil
}

// Ensure Provider satisfies the core interface at compile time. Watch is
// deliberately absent; see the package doc.
var _ mamori.Provider = (*Provider)(nil)
