// Package hcpvaultsecrets implements a mamori provider for HCP Vault Secrets
// (https://developer.hashicorp.com/hcp/docs/vault-secrets), the hosted
// secret-management service on HashiCorp Cloud Platform.
//
// # This is not providers/vault
//
// HCP Vault Secrets and self-hosted HashiCorp Vault are different products with
// different APIs, and this module covers only the first. providers/vault speaks
// Vault's own HTTP API (/v1/secret/data/..., the X-Vault-Token header, KV v2
// mounts) against a Vault cluster you run. This package speaks the HCP control
// plane at api.cloud.hashicorp.com, authenticates with an HCP service principal
// through OAuth2, and has no mounts, no policies, and no token renewal. Neither
// can resolve the other's refs, which is why the scheme is "hcp-vs" rather than
// any spelling of "vault".
//
// HashiCorp publishes a Go SDK (github.com/hashicorp/hcp-sdk-go), but it is not
// used here: the read path is a documented HTTPS API, so this provider is built
// on github.com/xavidop/mamori/providers/httpcore and the standard library
// only, keeping the SDK's dependency tree out of every consumer's build and
// inheriting request building, status classification, body bounding and URL
// redaction from one shared place.
//
// # Scheme
//
//	hcp-vs://<secretName>[#<key>][?<opts>]
//
// The ENTIRE ref path is the secret name, including any slashes it contains,
// matching providers/cloudflare-kv's and providers/infisical's precedent that a
// key with slashes is one key rather than a path. The organization, project and
// application that scope that name are therefore never taken from the path:
// they come from provider options, each overridable per ref with ?org=,
// ?project= and ?app=.
//
// # Authentication
//
// An HCP service principal key pair is exchanged for a short-lived access token
// through the standard RFC 6749 client-credentials grant, which is why this
// package reuses httpcore.OAuth2ClientCredentials rather than writing its own
// token authenticator the way providers/infisical had to. HCP's token endpoint
// takes an application/x-www-form-urlencoded body of grant_type, client_id,
// client_secret and audience, and answers with access_token, token_type and
// expires_in: exactly the shape httpcore already implements, including the
// optional audience parameter.
//
// Supply the pair with WithClientID/WithClientSecret or through HCP_CLIENT_ID
// and HCP_CLIENT_SECRET; an explicit option wins over its environment variable,
// and the environment is read lazily at resolve time so registering this
// provider from init is safe even when no credentials exist at process start.
//
// # Watching
//
// The HCP Vault Secrets read API exposes no streaming read, no blocking read,
// and no ETag this provider could gate a conditional GET on, so it deliberately
// does not implement mamori.WatchableProvider and mamori wraps it in the
// polling adapter instead, using Value.Version (the backend's own
// static_version.version) to detect a change between ticks.
package hcpvaultsecrets

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
	scheme = "hcp-vs"
	// defaultBaseURL is the HCP control plane, which serves the Vault Secrets
	// API. It is a different host from the identity provider below.
	defaultBaseURL = "https://api.cloud.hashicorp.com"
	// defaultTokenURL is HCP's OAuth2 token endpoint. It is deliberately NOT
	// the auth.hashicorp.com/oauth/token that HashiCorp's older support article
	// shows: that host expects a JSON body, while the current HCP API
	// documentation specifies this one with a form-encoded body. See the
	// README's "Pinned API contract" for both citations.
	defaultTokenURL = "https://auth.idp.hashicorp.com/oauth2/token"
	// tokenAudience is the OAuth2 audience HCP requires on the grant. Omitting
	// it yields a token that the control plane refuses.
	tokenAudience = "https://api.hashicorp.cloud"
	// apiVersion is the dated path segment HCP versions this API with. A bump
	// means a different response shape rather than the same one at a new URL,
	// which is why no WithAPIVersion option exists: 2023-06-13, the previous
	// version, spells the read path /apps/{app}/open/{name} and would not parse
	// with the envelope in resolve.go.
	apiVersion = "2023-11-28"
	// userAgent identifies this provider in HCP's request logs, so an operator
	// can tell mamori's traffic from an application's own.
	userAgent = "mamori-hcp-vault-secrets"
)

// Environment variables read lazily at resolve time, each only when the
// corresponding option was not supplied. The first two are the names
// HashiCorp's own CLI and documentation use for a service principal key pair,
// so an operator who can already run `hcp` needs no new variables.
const (
	envClientID     = "HCP_CLIENT_ID"
	envClientSecret = "HCP_CLIENT_SECRET"
	envOrganization = "HCP_ORGANIZATION_ID"
	envProject      = "HCP_PROJECT_ID"
	envApp          = "HCP_APP_NAME"
)

// Provider resolves hcp-vs:// refs against the HCP Vault Secrets API. It is
// safe for concurrent use.
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

	organizationID string
	projectID      string
	appName        string

	baseURL       string
	tokenURL      string
	allowInsecure bool
	httpClient    *http.Client

	// mu guards client, which is built on the first Resolve rather than in New
	// so that reading the environment (and validating the two URLs) happens
	// after process start. Registering from init must not require credentials
	// to already exist.
	mu     sync.Mutex
	client *httpcore.Client
}

// settings is the resolved, per-ref scope of one read. All three are required:
// HCP addresses a secret only as (organization, project, application, name),
// and there is no default for any of them.
type settings struct {
	organizationID string
	projectID      string
	appName        string
}

// Option configures a Provider.
type Option func(*Provider)

// WithClientID sets the HCP service principal's client id. An empty id is
// ignored so that it falls back to HCP_CLIENT_ID rather than pinning an
// unusable empty credential.
//
// This particular guard is belt and braces: clientFor reads the id through
// firstNonEmpty, which already skips an empty value, so removing the guard
// changes no behaviour and no test can tell the difference. It is kept for
// symmetry with WithClientSecret, whose guard is NOT redundant: that one stores
// a func, and a func returning "" is non-nil, so dropping it would pin an empty
// secret and defeat the environment fallback entirely.
func WithClientID(id string) Option {
	return func(p *Provider) {
		if id != "" {
			p.clientID = id
		}
	}
}

// WithClientSecret sets the HCP service principal's client secret.
//
// The secret is captured in a closure rather than stored in a field, for the
// reason Provider's own doc comment gives: reflection reaches an unexported
// field and no String method can stop it. An empty secret is ignored so it
// falls back to HCP_CLIENT_SECRET.
func WithClientSecret(secret string) Option {
	return func(p *Provider) {
		if secret != "" {
			p.clientSecret = func() string { return secret }
		}
	}
}

// WithOrganizationID sets the default HCP organization id for refs that carry
// no ?org= option. It is a UUID, visible in the HCP portal URL and from
// `hcp profile display`.
func WithOrganizationID(id string) Option { return func(p *Provider) { p.organizationID = id } }

// WithProjectID sets the default HCP project id for refs that carry no
// ?project= option. It is a UUID from the same two places as the organization
// id.
func WithProjectID(id string) Option { return func(p *Provider) { p.projectID = id } }

// WithAppName sets the default Vault Secrets application for refs that carry no
// ?app= option. An application is the namespace a secret name lives in, so one
// provider can serve several by letting each ref name its own.
func WithAppName(name string) Option { return func(p *Provider) { p.appName = name } }

// WithBaseURL overrides https://api.cloud.hashicorp.com, for a proxy or a test
// double. A trailing slash is trimmed so joining a path onto it never produces
// a double slash.
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

// WithTokenURL overrides https://auth.idp.hashicorp.com/oauth2/token, for a
// proxy or a test double.
//
// It is separate from WithBaseURL because HCP genuinely serves the two from
// different hosts: the identity provider issues the token, the control plane
// serves the secret. A single override would force a test double to impersonate
// both, and would silently send the client secret to whichever host an operator
// pointed the API at.
func WithTokenURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.tokenURL = u
		}
	}
}

// WithAllowInsecure permits an http:// base URL or token URL, and nothing else:
// a scheme that is neither http nor https is still refused.
//
// It takes no argument deliberately. The client-credentials grant POSTs the
// client secret in the form body, so a cleartext token URL hands that secret to
// anything on the path, and that secret is the key to every value the backend
// serves. An opt-in that cannot be switched on by a bool variable that happened
// to default to true is the safer shape for a decision of that weight.
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

// New constructs an HCP Vault Secrets provider. Without options it reads
// HCP_CLIENT_ID, HCP_CLIENT_SECRET, HCP_ORGANIZATION_ID, HCP_PROJECT_ID and
// HCP_APP_NAME lazily at resolve time, so it is safe to register from init even
// when no credentials exist at process start.
//
// It returns no error: every failure a constructor could report here (a missing
// credential, a URL with the wrong scheme) is reported by Resolve instead,
// wrapping mamori.ErrInvalid. `mamori doctor` resolves every ref before
// deployment, so a misconfiguration still surfaces before production, and
// keeping New single-valued is what lets a blank import register the scheme.
func New(opts ...Option) *Provider {
	p := &Provider{baseURL: defaultBaseURL, tokenURL: defaultTokenURL}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "hcp-vs".
//
// It is not "vault", and not "hcp-vault": providers/vault already owns the
// first for self-hosted Vault, and the second reads like a variant of it rather
// than the separate product it is. "hcp-vs" follows the two-part abbreviation
// every other secret manager here uses (aws-sm, gcp-sm, azure-kv, scaleway-sm).
func (p *Provider) Scheme() string { return scheme }

// secretNameOf returns the ref's secret name, which is the ENTIRE ref path.
//
// HCP Vault Secrets names are conventionally SCREAMING_SNAKE_CASE, and
// cloudflare-kv's precedent applies: splitting a path into "scope plus name"
// would silently misread one name containing a slash as two segments. The
// organization, project and application come from options and ref query
// parameters instead, never from the path.
func secretNameOf(ref mamori.Ref) (string, error) {
	name := strings.TrimPrefix(ref.Path, "/")
	if name == "" {
		return "", fmt.Errorf("mamori/hcp-vs: ref %q requires a secret name: %w", ref.Raw, mamori.ErrInvalid)
	}
	return name, nil
}

// settingsFor resolves the organization, project and application for one ref.
//
// Precedence is the ref option, then the provider option, then the environment
// variable: an operator who knows providers/cloudflare-kv's ?namespace= rule or
// providers/infisical's ?project= rule already knows this one. The environment
// is read here, per resolve, rather than in New, so a process that learns its
// configuration after start still works.
//
// Every missing value wraps mamori.ErrInvalid rather than mamori.ErrNotFound.
// That distinction is load bearing: ErrNotFound is the one kind that makes
// mamori apply a field's default, so reporting a misconfiguration as one would
// turn a typo into a silently defaulted value instead of an error.
func (p *Provider) settingsFor(ref mamori.Ref) (settings, error) {
	s := settings{
		organizationID: firstNonEmpty(ref.Opt("org"), p.organizationID, os.Getenv(envOrganization)),
		projectID:      firstNonEmpty(ref.Opt("project"), p.projectID, os.Getenv(envProject)),
		appName:        firstNonEmpty(ref.Opt("app"), p.appName, os.Getenv(envApp)),
	}
	// Each message names both the option and the environment variable that
	// would supply the value, and none echoes a credential.
	switch {
	case s.organizationID == "":
		return settings{}, fmt.Errorf("mamori/hcp-vs: no organization id; set %s, use WithOrganizationID, or add ?org= to the ref: %w",
			envOrganization, mamori.ErrInvalid)
	case s.projectID == "":
		return settings{}, fmt.Errorf("mamori/hcp-vs: no project id; set %s, use WithProjectID, or add ?project= to the ref: %w",
			envProject, mamori.ErrInvalid)
	case s.appName == "":
		return settings{}, fmt.Errorf("mamori/hcp-vs: no application name; set %s, use WithAppName, or add ?app= to the ref: %w",
			envApp, mamori.ErrInvalid)
	}
	return s, nil
}

// clientFor returns the shared httpcore.Client, building it on first use.
//
// Building it lazily is what lets New read no environment and return no error.
// The lock is held across construction, which is safe because construction
// performs no network call: the token exchange happens on the first Apply,
// inside Client.Do, long after this returns.
//
// A construction failure is deliberately NOT cached. A process whose
// credentials arrive after start (a mounted secret, a sidecar-populated
// environment) then succeeds on a later poll instead of being poisoned by the
// first attempt.
func (p *Provider) clientFor() (*httpcore.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}

	base := p.baseURL
	if base == "" {
		base = defaultBaseURL
	}
	if err := checkURLScheme("base URL", base, p.allowInsecure); err != nil {
		return nil, err
	}
	tokenURL := p.tokenURL
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}

	clientID := firstNonEmpty(p.clientID, os.Getenv(envClientID))
	secret := p.clientSecret
	if secret == nil {
		if fromEnv := os.Getenv(envClientSecret); fromEnv != "" {
			secret = func() string { return fromEnv }
		}
	}
	switch {
	case clientID == "":
		return nil, fmt.Errorf("mamori/hcp-vs: no client id; set %s or use WithClientID: %w", envClientID, mamori.ErrInvalid)
	case secret == nil:
		return nil, fmt.Errorf("mamori/hcp-vs: no client secret; set %s or use WithClientSecret: %w", envClientSecret, mamori.ErrInvalid)
	}

	// httpcore's own client-credentials authenticator, rather than a bespoke
	// one: HCP's grant IS RFC 6749, form-encoded, with the optional audience
	// parameter httpcore already supports. It also validates the token URL's
	// scheme itself, keeps the client secret and the access token in closures,
	// and collapses concurrent exchanges into one.
	auth, err := httpcore.OAuth2ClientCredentials(httpcore.OAuth2Config{
		TokenURL:      tokenURL,
		ClientID:      clientID,
		ClientSecret:  secret(),
		Audience:      tokenAudience,
		HTTPClient:    p.httpClient,
		AllowInsecure: p.allowInsecure,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/hcp-vs: building the token exchange: %w", err)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     base,
		HTTPClient:  p.httpClient,
		Auth:        auth,
		UserAgent:   userAgent,
		ErrorDetail: errorMessage,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/hcp-vs: building the API client: %w", err)
	}
	p.client = c
	return c, nil
}

// checkURLScheme rejects a URL this package will not talk to, naming which of
// the two it was so an operator knows which option to fix.
//
// The scheme is checked against a closed set rather than only rejecting
// http://, for the reason providers/https gives for the same check:
// httpcore.New requires only a scheme and a host, so an ftp:// typo or a ws://
// paste constructs cleanly and then fails on every single resolve with
// net/http's "unsupported protocol scheme". Naming the closed set turns that
// into one legible error at the first resolve, where `mamori doctor` sees it.
func checkURLScheme(what, raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		// The unparsed string is not echoed: a URL can carry userinfo.
		return fmt.Errorf("mamori/hcp-vs: %s is not a URL: %w: %w", what, mamori.ErrInvalid, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf("mamori/hcp-vs: %s %s is http://, which sends the access token in cleartext; use WithAllowInsecure to accept that: %w",
				what, redactURL(u), mamori.ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("mamori/hcp-vs: %s scheme %q is not http or https: %w", what, u.Scheme, mamori.ErrInvalid)
	}
}

// redactURL renders a URL for an error message with its userinfo and query
// stripped, because a URL an operator pasted may carry either and this package
// must not put a credential into an error.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// errorMessage is httpcore's Config.ErrorDetail hook for the read path: it
// lifts HCP's "message" field out of a failing response so an operator sees WHY
// a request was refused rather than only its status.
//
// Only "message" is selected, never the whole body and never a sibling field.
// A response body can be the resolved secret itself, so httpcore leaves this
// hook nil by default and makes each provider decide; on the read path the
// error envelope is the googlerpcStatus shape {"code":..,"message":"..",
// "details":[..]} and the message names the resource and the reason, not a
// value. "details" is deliberately left alone: it is an open list of arbitrary
// typed payloads, so nothing bounds what a future one could contain.
//
// A message that is not a string fails to unmarshal and yields "", which
// suppresses the detail for that one response rather than guessing at a shape.
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
