// Package infisical implements a mamori provider for Infisical
// (https://infisical.com), the open-source secret manager.
//
// Infisical publishes a Go SDK, but it is not used here: the read path is a
// documented HTTPS API, so this provider is built on
// github.com/xavidop/mamori/providers/httpcore and the standard library only,
// keeping the SDK's dependency tree out of every consumer's build and
// inheriting request building, status classification, body bounding and URL
// redaction from one shared place.
//
// # Scheme
//
//	infisical://<secretName>[#<key>][?<opts>]
//
// The ENTIRE ref path is the secret name, including any slashes it contains,
// matching providers/cloudflare-kv's precedent that a key with slashes is one
// key rather than a path. The project, environment and secret path that scope
// that name are therefore never taken from the path: they come from provider
// options, each overridable per ref with ?project=, ?env= and ?path=.
//
// # Authentication
//
// Machine identity Universal Auth: a client id and client secret are exchanged
// for a short-lived access token, which is cached and refreshed before expiry.
// Supply them with WithClientID/WithClientSecret or through INFISICAL_CLIENT_ID
// and INFISICAL_CLIENT_SECRET; an explicit option wins over its environment
// variable, and the environment is read lazily at resolve time so registering
// this provider from init is safe even when no credentials exist at process
// start.
//
// httpcore.OAuth2ClientCredentials is deliberately NOT used. It posts the RFC
// 6749 client-credentials form (grant_type, client_id, client_secret) and reads
// access_token/expires_in; Infisical posts JSON {clientId, clientSecret} to a
// non-standard path and answers with accessToken/expiresIn. The two shapes do
// not overlap, so this package writes its own token authenticator in auth.go,
// following httpcore's structure decision for decision.
//
// # Watching
//
// The Infisical read API exposes no streaming read, no blocking read, and no
// ETag this provider could gate a conditional GET on, so it deliberately does
// not implement mamori.WatchableProvider and mamori wraps it in the polling
// adapter instead, using Value.Version (the backend's own revision number) to
// detect a change between ticks.
package infisical

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

const (
	scheme = "infisical"
	// defaultBaseURL is Infisical Cloud. A self-hosted install overrides it
	// with WithBaseURL.
	defaultBaseURL = "https://app.infisical.com"
	// loginPath is the Universal Auth login endpoint. It is v1 while the read
	// path below is v4: Infisical versions the two independently, and this is
	// the pairing its reference documents.
	loginPath = "/api/v1/auth/universal-auth/login"
	// secretsPath is the prefix of the single-secret read endpoint. It is v4,
	// not the v3 most third-party write-ups still describe, and a version bump
	// means a different response shape rather than the same one at a new URL,
	// which is why no WithAPIVersion option exists.
	secretsPath = "/api/v4/secrets/"
	// defaultSecretPath is the folder a ref addresses when neither the ref, an
	// option, nor the environment names one. It matches the API's own default.
	defaultSecretPath = "/"
	// userAgent identifies this provider in Infisical's request logs, so an
	// operator can tell mamori's traffic from an application's own.
	userAgent = "mamori-infisical"
)

// Environment variables read lazily at resolve time, each only when the
// corresponding option was not supplied.
const (
	envClientID     = "INFISICAL_CLIENT_ID"
	envClientSecret = "INFISICAL_CLIENT_SECRET"
	envProjectID    = "INFISICAL_PROJECT_ID"
	envEnvironment  = "INFISICAL_ENVIRONMENT"
	envSecretPath   = "INFISICAL_SECRET_PATH"
)

// Provider resolves infisical:// refs against Infisical's REST API. It is safe
// for concurrent use.
//
// The client secret is held as a closure rather than a string field, and that
// is not decoration. fmt's %v, %+v and %#v walk unexported struct fields by
// reflection, and reflection cannot call a String or GoString method on a value
// it reaches that way, so no redaction method could stop a debug dump or a
// panic trace printing a plain field in cleartext. A closure renders as an
// opaque function pointer instead. httpcore's own OAuth2 authenticator makes
// the same choice for the same reason; a Provider is if anything MORE likely to
// be printed, since it is the value an application passes to
// mamori.WithProvider.
type Provider struct {
	clientID string
	// clientSecret returns the configured client secret, or is nil when none
	// was supplied and the environment must be consulted instead.
	clientSecret func() string

	projectID   string
	environment string
	secretPath  string

	baseURL       string
	allowInsecure bool
	httpClient    *http.Client

	// mu guards client and closed. client is built on the first Resolve rather
	// than in New so that reading the environment (and validating the base URL)
	// happens after process start. Registering from init must not require
	// credentials to already exist.
	mu     sync.Mutex
	client *httpcore.Client
	closed bool
}

// settings is the resolved, per-ref scope of one read: which project, which
// environment, and which folder inside it.
type settings struct {
	projectID   string
	environment string
	secretPath  string
}

// Option configures a Provider.
type Option func(*Provider)

// WithClientID sets the machine identity's client id used for Universal Auth.
// An empty id is ignored so that it falls back to INFISICAL_CLIENT_ID rather
// than pinning an unusable empty credential.
func WithClientID(id string) Option {
	return func(p *Provider) {
		if id != "" {
			p.clientID = id
		}
	}
}

// WithClientSecret sets the machine identity's client secret.
//
// The secret is captured in a closure rather than stored in a field, for the
// reason Provider's own doc comment gives: reflection reaches an unexported
// field and no String method can stop it. An empty secret is ignored so it
// falls back to INFISICAL_CLIENT_SECRET.
func WithClientSecret(secret string) Option {
	return func(p *Provider) {
		if secret != "" {
			p.clientSecret = func() string { return secret }
		}
	}
}

// WithProjectID sets the default Infisical project id for refs that carry no
// ?project= option. A project id is required: Infisical scopes every secret
// name to one, so without it a name is ambiguous.
func WithProjectID(id string) Option { return func(p *Provider) { p.projectID = id } }

// WithEnvironment sets the default environment slug ("dev", "prod", ...) for
// refs that carry no ?env= option. It is optional because the API treats it as
// optional; omitting it lets one provider serve a project whose environment is
// chosen per ref.
func WithEnvironment(slug string) Option { return func(p *Provider) { p.environment = slug } }

// WithSecretPath sets the default folder for refs that carry no ?path= option.
// It defaults to "/", matching the API, so a flat project needs no
// configuration at all.
func WithSecretPath(path string) Option { return func(p *Provider) { p.secretPath = path } }

// WithBaseURL overrides https://app.infisical.com for a self-hosted install or
// a test double. A trailing slash is trimmed so joining a path onto it never
// produces a double slash.
//
// The scheme is validated on the first Resolve, not here: an Option cannot
// return an error without making New return one too, and New must stay
// single-valued so init can register a provider with it.
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithAllowInsecure permits an http:// base URL, and nothing else: a scheme
// that is neither http nor https is still refused.
//
// It takes no argument deliberately. Universal Auth POSTs the client secret in
// the request body, so a cleartext base URL hands that secret to anything on
// the path, and that secret is the key to every value the backend serves. An
// opt-in that cannot be switched on by a bool variable that happened to default
// to true is the safer shape for a decision of that weight.
func WithAllowInsecure() Option { return func(p *Provider) { p.allowInsecure = true } }

// WithHTTPClient injects a custom *http.Client, for a proxy, a custom
// transport, or an in-process test fake. A nil client is a no-op so a caller
// cannot accidentally erase the default. The same client serves both the token
// exchange and the secret read, so a transport-level policy applies to each.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs an Infisical provider. Without options it reads
// INFISICAL_CLIENT_ID, INFISICAL_CLIENT_SECRET, INFISICAL_PROJECT_ID,
// INFISICAL_ENVIRONMENT and INFISICAL_SECRET_PATH lazily at resolve time, so it
// is safe to register from init even when no credentials exist at process
// start.
//
// It returns no error: every failure a constructor could report here (a missing
// credential, a base URL with the wrong scheme) is reported by Resolve instead,
// wrapping mamori.ErrInvalid. `mamori doctor` resolves every ref before
// deployment, so a misconfiguration still surfaces before production, and
// keeping New single-valued is what lets a blank import register the scheme.
func New(opts ...Option) *Provider {
	p := &Provider{baseURL: defaultBaseURL}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "infisical".
func (p *Provider) Scheme() string { return scheme }

// secretNameOf returns the ref's secret name, which is the ENTIRE ref path.
//
// Infisical secret names are conventionally SCREAMING_SNAKE_CASE, but the API
// does not forbid a slash, and cloudflare-kv's precedent applies: splitting a
// path into "scope plus name" would silently misread one name containing a
// slash as two segments. The project, environment and folder come from options
// and ref query parameters instead, never from the path.
func secretNameOf(ref mamori.Ref) (string, error) {
	name := strings.TrimPrefix(ref.Path, "/")
	if name == "" {
		return "", fmt.Errorf("mamori/infisical: ref %q requires a secret name: %w", ref.Raw, mamori.ErrInvalid)
	}
	return name, nil
}

// settingsFor resolves the project, environment and folder for one ref.
//
// Precedence is the ref option, then the provider option, then the environment
// variable: an operator who knows providers/cloudflare-kv's ?namespace= rule
// already knows this one. The environment is read here, per resolve, rather
// than in New, so a process that learns its credentials after start still
// works.
//
// A missing project id wraps mamori.ErrInvalid rather than mamori.ErrNotFound.
// That distinction is load bearing: ErrNotFound is the one kind that makes
// mamori apply a field's default, so reporting a misconfiguration as one would
// turn a typo into a silently defaulted value instead of an error.
func (p *Provider) settingsFor(ref mamori.Ref) (settings, error) {
	s := settings{
		projectID:   firstNonEmpty(ref.Opt("project"), p.projectID, os.Getenv(envProjectID)),
		environment: firstNonEmpty(ref.Opt("env"), p.environment, os.Getenv(envEnvironment)),
		secretPath:  firstNonEmpty(ref.Opt("path"), p.secretPath, os.Getenv(envSecretPath), defaultSecretPath),
	}
	if s.projectID == "" {
		return settings{}, fmt.Errorf("mamori/infisical: no project id; set %s, use WithProjectID, or add ?project= to the ref: %w",
			envProjectID, mamori.ErrInvalid)
	}
	return s, nil
}

// clientFor returns the shared httpcore.Client, building it on first use.
//
// Building it lazily is what lets New read no environment and return no error.
// The lock is held across construction, which is safe because construction
// performs no network call: the Universal Auth exchange happens on the first
// Apply, inside Client.Do, long after this returns.
//
// A construction failure is deliberately NOT cached. A process whose
// credentials arrive after start (a mounted secret, a sidecar-populated
// environment) then succeeds on a later poll instead of being poisoned by the
// first attempt.
//
// The closed check runs first, ahead of the p.client != nil cache check, so a
// closed provider refuses locally even when a client built before Close is
// still sitting in p.client.
func (p *Provider) clientFor() (*httpcore.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("%w: infisical: provider is closed", mamori.ErrUnavailable)
	}
	if p.client != nil {
		return p.client, nil
	}

	base := p.baseURL
	if base == "" {
		base = defaultBaseURL
	}
	if err := checkBaseURLScheme(base, p.allowInsecure); err != nil {
		return nil, err
	}

	clientID := firstNonEmpty(p.clientID, os.Getenv(envClientID))
	secret := p.clientSecret
	if secret == nil {
		if fromEnv := os.Getenv(envClientSecret); fromEnv != "" {
			secret = func() string { return fromEnv }
		}
	}
	// Each message names both the option and the environment variable that
	// would supply the value, and neither echoes a credential that IS set.
	switch {
	case clientID == "":
		return nil, fmt.Errorf("mamori/infisical: no client id; set %s or use WithClientID: %w", envClientID, mamori.ErrInvalid)
	case secret == nil:
		return nil, fmt.Errorf("mamori/infisical: no client secret; set %s or use WithClientSecret: %w", envClientSecret, mamori.ErrInvalid)
	}

	// The login client carries no Authenticator (the exchange is what produces
	// the credential) and no ErrorDetail hook. Leaving ErrorDetail nil on this
	// one client is deliberate: it is the only response in this provider whose
	// body is a reply to a request that CONTAINED the client secret, and no
	// vendor guarantee says an error envelope cannot echo part of what it
	// rejected.
	loginClient, err := httpcore.New(httpcore.Config{
		BaseURL:    base,
		HTTPClient: p.httpClient,
		UserAgent:  userAgent,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/infisical: building the login client: %w", err)
	}

	auth, err := newUniversalAuth(universalAuthConfig{
		client:       loginClient,
		clientID:     clientID,
		clientSecret: secret,
	})
	if err != nil {
		return nil, err
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     base,
		HTTPClient:  p.httpClient,
		Auth:        auth,
		UserAgent:   userAgent,
		ErrorDetail: errorMessage,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/infisical: building the API client: %w", err)
	}
	p.client = c
	return c, nil
}

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, through the same closed check
// clientFor already applies, without contacting Infisical.
//
// A client supplied through WithHTTPClient is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the caller's
// own use of that client is unaffected by closing this provider.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.httpClient != nil {
		p.httpClient.CloseIdleConnections()
	}
	return nil
}

// checkBaseURLScheme rejects a base URL this package will not POST a client
// secret to.
//
// The scheme is checked against a closed set rather than only rejecting
// http://, for the reason providers/https gives for the same check: httpcore.New
// requires only a scheme and a host, so an ftp:// typo or a ws:// paste
// constructs cleanly and then fails on every single resolve with net/http's
// "unsupported protocol scheme". Naming the closed set turns that into one
// legible error.
func checkBaseURLScheme(base string, allowInsecure bool) error {
	u, err := url.Parse(base)
	if err != nil {
		// The unparsed string is not echoed: a URL can carry userinfo.
		return fmt.Errorf("mamori/infisical: base URL is not a URL: %w: %w", mamori.ErrInvalid, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf("mamori/infisical: base URL %s is http://, which POSTs the client secret in cleartext; use WithAllowInsecure to accept that: %w",
				redactURL(u), mamori.ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("mamori/infisical: base URL scheme %q is not http or https: %w", u.Scheme, mamori.ErrInvalid)
	}
}

// redactURL renders a URL for an error message with its userinfo and query
// stripped, because a base URL an operator pasted may carry either and this
// package must not put a credential into an error.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// errorMessage is httpcore's Config.ErrorDetail hook for the read path: it
// lifts Infisical's "message" field out of a failing response so an operator
// sees WHY a request was refused rather than only its status.
//
// Only "message" is selected, never the whole body and never a sibling field.
// A response body can be the resolved secret itself, so httpcore leaves this
// hook nil by default and makes each provider decide; on the read path the
// error envelope is {"statusCode":..,"message":"..","error":".."} and the
// message names the secret and the reason, not a value.
//
// A message that is not a string (Infisical answers some validation failures
// with an array of messages) fails to unmarshal and yields "", which suppresses
// the detail for that one response rather than guessing at a shape.
func errorMessage(_ int, body []byte) string {
	var env struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.Message
}

// firstNonEmpty returns the first non-empty string among vals, or "" if all are
// empty. It is what encodes every precedence chain in this package.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
