// Package heroku implements a mamori provider for Heroku config vars
// (https://devcenter.heroku.com/articles/config-vars), the environment-variable
// store the Heroku platform injects into every dyno of an app.
//
// Heroku publishes no official Go client for the Platform API, and none is
// needed: the read path is one documented HTTPS GET, so this provider is built
// on github.com/xavidop/mamori/providers/httpcore and the standard library
// only, inheriting request building, status classification, body bounding and
// URL redaction from one shared place.
//
// # Scheme
//
//	heroku://<VAR>[#<key>]              the app comes from configuration
//	heroku://<app>/<VAR>[#<key>]        the app is named in the ref
//
// <app> is Heroku's {app_id_or_name}: either an app name or its UUID. <VAR> is
// the config var name. A third path segment is refused rather than guessed at.
//
// The app follows providers/cloudflare-kv's precedence rule - the ref wins over
// the provider option, which wins over the environment - with one deliberate
// difference in HOW the ref says it. cloudflare-kv must use a ?namespace= query
// option because a Workers KV key may itself contain slashes, so a path could
// not be split without misreading such a key as two segments. A Heroku config
// var name cannot: the vendor's own JSON schema constrains a config var key to
// the pattern ^\w+$ and the Dev Center tells operators to use "only
// alphanumeric characters and the underscore character". A slash in a heroku://
// ref path is therefore unambiguous, and the readable heroku://<app>/<VAR> form
// costs nothing.
//
// That schema pattern is NOT enforced here. It is a vendor-side rule, and
// re-implementing it client-side would turn "the backend would have told you"
// into "mamori refused before asking", which is strictly worse: it can only
// ever reject a name the backend might have accepted.
//
// # Authentication
//
// A Heroku API token in the Authorization header, from WithAPIKey or from
// HEROKU_API_KEY - the same variable the Heroku CLI reads, so a machine already
// set up to run `heroku config` needs nothing further. Mint one with
// `heroku authorizations:create`.
//
// Every request also carries the Accept version header the Platform API
// requires (see acceptVersion). Omitting it is the single most common way a
// hand-rolled Heroku integration fails, and it fails as a 406 that says nothing
// about the app or the token.
//
// # Batching
//
// One GET returns EVERY config var of an app in one document, so this provider
// implements mamori.BatchProvider: a config with twelve heroku:// fields costs
// one request per app rather than twelve. See ResolveBatch.
//
// # Watching
//
// The Platform API exposes no streaming or blocking read of config vars, so
// this provider deliberately does not implement mamori.WatchableProvider and
// mamori wraps it in the polling adapter instead, using Value.Version to detect
// a change between ticks.
package heroku

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
	scheme = "heroku"
	// defaultBaseURL is the Platform API origin. The reference states that
	// "Clients must address requests to api.heroku.com using HTTPS".
	defaultBaseURL = "https://api.heroku.com"
	// appsPath and configVarsSuffix bracket the app identity in the documented
	// path template, GET /apps/{app_id_or_name}/config-vars. They are kept as
	// two constants rather than one format string because the identity between
	// them must be url.PathEscape'd, and a format string invites writing it in
	// raw.
	appsPath         = "/apps/"
	configVarsSuffix = "/config-vars"
	// acceptVersion is the Accept header the Platform API REQUIRES on every
	// request: "Clients must address requests to api.heroku.com using HTTPS and
	// specify the Accept: application/vnd.heroku+json; version=3 Accept
	// header." A request without it is answered 406 not_acceptable, whose
	// message ("request failed, set Accept: ... header and try again") mentions
	// neither the app nor the token, which is why this is the failure mode a
	// hand-rolled Heroku client hits first and understands last.
	//
	// There is no WithAPIVersion option on purpose. A version bump changes the
	// response shape, not only the URL, so a provider that let an operator ask
	// for a version it cannot parse would trade one legible failure for an
	// illegible one.
	acceptVersion = "application/vnd.heroku+json; version=3"
	// userAgent identifies this provider in Heroku's request logs, so an
	// operator can tell mamori's traffic from their own application's.
	userAgent = "mamori-heroku"
)

// Environment variables read lazily at resolve time, each only when the
// corresponding option was not supplied.
const (
	// envAPIKey is the variable the Heroku CLI itself reads for a token, so a
	// host already configured for `heroku config` needs no new secret.
	envAPIKey = "HEROKU_API_KEY"
	// envApp is the variable the Heroku CLI reads for a default app, the
	// equivalent of its -a flag.
	envApp = "HEROKU_APP"
	// envAppName is injected by the platform itself into every dyno once
	// `heroku labs:enable runtime-dyno-metadata` is on. It is consulted LAST,
	// below envApp, precisely because nobody chose it: a process running on
	// Heroku reading its own app's config vars is a sensible default, but an
	// operator who named a different app meant the one they named.
	envAppName = "HEROKU_APP_NAME"
)

// Provider resolves heroku:// refs against the Heroku Platform API. It is safe
// for concurrent use.
//
// The API token is held as a closure rather than a string field, and that is
// not decoration. fmt's %v, %+v and %#v walk unexported struct fields by
// reflection, and reflection cannot call a String or GoString method on a value
// it reaches that way, so no redaction method could stop a debug dump or a
// panic trace printing a plain field in cleartext. A closure renders as an
// opaque function pointer instead. httpcore's OAuth2 authenticator and
// providers/infisical both make the same choice for the same reason, and a
// Provider is if anything MORE likely to be printed, since it is the value an
// application hands to mamori.WithProvider.
type Provider struct {
	// apiKey returns the configured token, or is nil when none was supplied and
	// the environment must be consulted instead.
	apiKey func() string

	app           string
	baseURL       string
	allowInsecure bool
	httpClient    *http.Client

	// mu guards client, which is built on the first resolve rather than in New
	// so that reading the environment (and validating the base URL) happens
	// after process start. Registering from init must not require a token to
	// already exist.
	mu     sync.Mutex
	client *httpcore.Client
}

// target is the app and config var name one ref addresses. Every read is scoped
// by both: a config var name means nothing without the app that owns it.
type target struct {
	app  string
	name string
}

// Option configures a Provider.
type Option func(*Provider)

// WithAPIKey sets the Heroku API token used to authenticate requests.
//
// The token is captured in a closure rather than stored in a field, for the
// reason Provider's own doc comment gives. An empty token is ignored so that it
// falls back to HEROKU_API_KEY rather than pinning an unusable empty credential.
func WithAPIKey(token string) Option {
	return func(p *Provider) {
		if token != "" {
			p.apiKey = func() string { return token }
		}
	}
}

// WithApp sets the default app (name or UUID) for refs that name none
// themselves. A ref written as heroku://<app>/<VAR> still wins over it, so one
// provider can serve refs spanning several apps.
func WithApp(app string) Option { return func(p *Provider) { p.app = app } }

// WithBaseURL overrides https://api.heroku.com, for a proxy or a test double. A
// trailing slash is trimmed so joining a path onto it never produces a double
// slash.
//
// The scheme is validated on the first resolve, not here: an Option cannot
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
// It takes no argument deliberately. The API token travels in an Authorization
// header on every request, so a cleartext base URL hands it to anything on the
// path, and that one token reaches every config var of every app the account
// can see. An opt-in that cannot be switched on by a bool variable that
// happened to default to true is the safer shape for a decision of that weight.
func WithAllowInsecure() Option { return func(p *Provider) { p.allowInsecure = true } }

// WithHTTPClient injects a custom *http.Client, for a proxy, a custom
// transport, or an in-process test fake. A nil client is a no-op so a caller
// cannot accidentally erase the default.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a Heroku config vars provider. Without options it reads
// HEROKU_API_KEY, HEROKU_APP and HEROKU_APP_NAME lazily at resolve time, so it
// is safe to register from init even when no credentials exist at process start.
//
// It returns no error: every failure a constructor could report here (a missing
// token, a base URL with the wrong scheme) is reported by Resolve instead,
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

// Scheme returns "heroku".
func (p *Provider) Scheme() string { return scheme }

// targetFor resolves the app and config var name one ref addresses.
//
// The grammar is total: one segment is a config var name, two are an app and a
// name, and anything else is refused with mamori.ErrInvalid rather than having
// a segment silently ignored. A ref that quietly means something other than
// what it says is worse than one that fails, and `mamori doctor` resolves every
// ref before deployment, so a refusal surfaces there rather than in production.
//
// Precedence for the app is the ref, then WithApp, then HEROKU_APP, then
// HEROKU_APP_NAME - providers/cloudflare-kv's rule, with the ref-level source
// expressed as a path segment rather than a query option (see the package doc
// for why that is safe here and is not there). The environment is read per
// resolve rather than in New, so a process that learns its configuration after
// start still works.
//
// A missing app wraps mamori.ErrInvalid rather than mamori.ErrNotFound. That
// distinction is load bearing: ErrNotFound is the one kind that makes mamori
// apply a field's default, so reporting a misconfiguration as one would turn a
// forgotten HEROKU_APP into a silently defaulted value instead of an error.
func (p *Provider) targetFor(ref mamori.Ref) (target, error) {
	path := strings.TrimPrefix(ref.Path, "/")
	if path == "" {
		return target{}, fmt.Errorf("mamori/heroku: ref %q requires a config var name: %w", ref.Raw, mamori.ErrInvalid)
	}

	var app, name string
	parts := strings.Split(path, "/")
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		app, name = parts[0], parts[1]
	default:
		return target{}, fmt.Errorf("mamori/heroku: ref %q has %d path segments; the grammar is heroku://<VAR> or heroku://<app>/<VAR>: %w",
			ref.Raw, len(parts), mamori.ErrInvalid)
	}
	if name == "" {
		return target{}, fmt.Errorf("mamori/heroku: ref %q requires a config var name after the app: %w", ref.Raw, mamori.ErrInvalid)
	}

	app = firstNonEmpty(app, p.app, os.Getenv(envApp), os.Getenv(envAppName))
	if app == "" {
		return target{}, fmt.Errorf("mamori/heroku: ref %q names no app; write heroku://<app>/%s, set %s, or use WithApp: %w",
			ref.Raw, name, envApp, mamori.ErrInvalid)
	}
	return target{app: app, name: name}, nil
}

// clientFor returns the shared httpcore.Client, building it on first use.
//
// Building it lazily is what lets New read no environment and return no error.
// The lock is held across construction, which is safe because construction
// performs no network call.
//
// A construction failure is deliberately NOT cached. A process whose token
// arrives after start (a mounted secret, a sidecar-populated environment) then
// succeeds on a later poll instead of being poisoned by the first attempt.
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
	if err := checkBaseURLScheme(base, p.allowInsecure); err != nil {
		return nil, err
	}

	token := p.apiKey
	if token == nil {
		if fromEnv := os.Getenv(envAPIKey); fromEnv != "" {
			token = func() string { return fromEnv }
		}
	}
	if token == nil {
		// The message names both the option and the environment variable that
		// would supply the value, and echoes no credential.
		return nil, fmt.Errorf("mamori/heroku: no API token; set %s or use WithAPIKey: %w", envAPIKey, mamori.ErrInvalid)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:    base,
		HTTPClient: p.httpClient,
		// httpcore.Bearer is exactly Heroku's documented form,
		// "Authorization: Bearer {HEROKU_TOKEN}". The token is read out of the
		// closure once, here, so it still never lives in a struct field.
		Auth:        httpcore.Bearer(token()),
		UserAgent:   userAgent,
		ErrorDetail: errorID,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/heroku: building the API client: %w", err)
	}
	p.client = c
	return c, nil
}

// checkBaseURLScheme rejects a base URL this package will not send an API token
// to.
//
// The scheme is checked against a closed set rather than only rejecting http://,
// for the reason providers/https and providers/infisical give for the same
// check: httpcore.New requires only a scheme and a host, so an ftp:// typo or a
// ws:// paste constructs cleanly and then fails on every single resolve with
// net/http's "unsupported protocol scheme". Naming the closed set turns that
// into one legible error.
func checkBaseURLScheme(base string, allowInsecure bool) error {
	u, err := url.Parse(base)
	if err != nil {
		// The unparsed string is not echoed: a URL can carry userinfo.
		return fmt.Errorf("mamori/heroku: base URL is not a URL: %w: %w", mamori.ErrInvalid, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf("mamori/heroku: base URL %s is http://, which sends the API token in cleartext; use WithAllowInsecure to accept that: %w",
				redactURL(u), mamori.ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("mamori/heroku: base URL scheme %q is not http or https: %w", u.Scheme, mamori.ErrInvalid)
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

// errorID is httpcore's Config.ErrorDetail hook: it lifts Heroku's error "id"
// out of a failing response so an operator sees WHY a request was refused
// rather than only its status.
//
// It selects "id" and NOT the sibling "message", which is the more obvious
// choice and the wrong one. Heroku documents "id" as a closed vocabulary -
// bad_request, unauthorized, delinquent, forbidden, suspended, not_found,
// not_acceptable, conflict, gone, requested_range_not_satisfiable,
// invalid_params, verification_needed, rate_limit - so it is a value from a
// fixed list and can never carry anything the caller sent or anything the app
// stores. "message" is free prose the vendor may reword at any time, and on the
// success path the body of this very endpoint is the app's ENTIRE config var
// document: every credential the app holds. httpcore leaves this hook nil by
// default for exactly that reason, and "the field that cannot contain a value"
// is a stronger guarantee than "the field that currently does not".
//
// An "id" that is not a string, or a body that is not JSON at all (a proxy's
// HTML error page), fails to unmarshal and yields "", suppressing the detail
// for that one response rather than guessing at a shape.
func errorID(_ int, body []byte) string {
	var env struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.ID
}

// firstNonEmpty returns the first non-empty string among vals, or "" if all are
// empty. It is what encodes the app precedence chain in this package.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
