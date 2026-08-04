// Package supabase implements a mamori provider that resolves secrets from
// Supabase Vault (https://supabase.com/docs/guides/database/vault) over the
// project's PostgREST Data API.
//
// Supabase publishes a Go client, and the project also ships providers/postgres
// for direct SQL. Neither is used here: this is the REST path, so the provider
// is built on github.com/xavidop/mamori/providers/httpcore and the standard
// library only, keeping every SDK and every Postgres driver out of a consumer's
// build and inheriting request building, status classification, body bounding
// and URL redaction from one shared place.
//
// # This provider CANNOT read vault.decrypted_secrets directly
//
// That is the single most important thing to know about it, and it is a
// property of Supabase rather than a choice made here.
//
// Supabase Vault stores secrets in vault.secrets and exposes their plaintext
// through the vault.decrypted_secrets view. The vault schema is NOT exposed
// over the Data API, and unlike an ordinary custom schema it cannot be added to
// the dashboard's "Exposed schemas" list: auth, storage, realtime and vault are
// restricted precisely so that third-party tooling cannot reach them. There is
// therefore no combination of Accept-Profile header and API key that reads
// vault.decrypted_secrets over PostgREST.
//
// So this provider reads a relation the OPERATOR creates in a schema that IS
// exposed, which re-exposes vault.decrypted_secrets under controlled
// privileges. The provider's README carries the exact SQL. Without that setup
// this provider cannot work at all, and it says so in its errors rather than
// pretending otherwise: PostgREST answers an unknown relation with 404, which
// mamori classifies as not_found.
//
// # Scheme
//
//	supabase://<secretName>[#<key>][?<opts>]
//
// The ENTIRE ref path is the secret name, including any slashes or dots it
// contains. Unlike providers/infisical, the name never reaches the request
// PATH: it travels as a PostgREST filter value in the query string, so a name
// is always one literal name and never a traversal. The schema and relation
// that scope it come from provider options, each overridable per ref with
// ?schema= and ?view=.
//
// # Authentication
//
// Two headers on every request, both carrying the same key: apikey, which is
// what the Supabase gateway authenticates, and Authorization: Bearer, which is
// what PostgREST reads to choose the database role.
//
// That key must be the project's service-role (secret) key, not the anon
// (publishable) one. A relation that re-exposes decrypted secrets should be
// granted to service_role alone, so an anon key is refused by design; and
// granting it to anon would publish every secret in the Vault to the public
// internet, since the anon key ships in browsers. Supply the key with
// WithServiceKey or through SUPABASE_SERVICE_ROLE_KEY.
//
// # Watching
//
// PostgREST exposes no streaming read, no blocking read, and no ETag this
// provider could gate a conditional GET on, so it deliberately does not
// implement mamori.WatchableProvider and mamori wraps it in the polling adapter
// instead, using Value.Version (the row's updated_at) to detect a change
// between ticks.
package supabase

import (
	"context"
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
	scheme = "supabase"
	// restPrefix is the path PostgREST is mounted at on every Supabase
	// project. It is joined onto the project URL rather than asked for
	// separately, because an operator copies the project URL out of the
	// dashboard and the mount point is not theirs to choose.
	restPrefix = "/rest/v1"
	// defaultSchema is the schema the operator's relation is looked for in.
	// public is the one schema every project exposes out of the box, so a
	// setup that follows the README needs no schema configuration at all.
	defaultSchema = "public"
	// defaultView is the relation name the README's SQL creates. It matches
	// the Vault view it re-exposes, so an operator reading a ref can tell what
	// it ultimately reads.
	defaultView = "decrypted_secrets"
	// nameColumn and secretColumn are the columns the operator's relation must
	// expose, and they are deliberately NOT configurable. They are the names
	// vault.decrypted_secrets itself uses, so the README's "select these
	// columns from the Vault view" SQL produces them for free; a knob here
	// would only let a relation drift from the view it mirrors.
	nameColumn   = "name"
	secretColumn = "decrypted_secret"
	// versionColumn is the row's last-write timestamp, used as Value.Version.
	versionColumn = "updated_at"
	// userAgent identifies this provider in a project's request logs, so an
	// operator can tell mamori's traffic from an application's own.
	userAgent = "mamori-supabase"
)

// Environment variables read lazily at resolve time, each only when the
// corresponding option was not supplied.
const (
	envURL        = "SUPABASE_URL"
	envServiceKey = "SUPABASE_SERVICE_ROLE_KEY"
	envSchema     = "SUPABASE_VAULT_SCHEMA"
	envView       = "SUPABASE_VAULT_VIEW"
)

// Provider resolves supabase:// refs against a project's PostgREST Data API.
// It is safe for concurrent use.
//
// The service key is held as a closure rather than a string field, and that is
// not decoration. fmt's %v, %+v and %#v walk unexported struct fields by
// reflection, and reflection cannot call a String or GoString method on a value
// it reaches that way, so no redaction method could stop a debug dump or a
// panic trace printing a plain field in cleartext. A closure renders as an
// opaque function pointer instead. The stake is unusually high for this
// provider: a service-role key bypasses every row-level security policy on the
// whole project, so it is not merely one secret but the key to all of them.
type Provider struct {
	projectURL string
	// serviceKey returns the configured service-role key, or is nil when none
	// was supplied and the environment must be consulted instead.
	serviceKey func() string

	schema string
	view   string

	allowInsecure bool
	httpClient    *http.Client

	// mu guards client, which is built on the first Resolve rather than in New
	// so that reading the environment (and validating the project URL) happens
	// after process start. Registering from init must not require credentials
	// to already exist.
	mu     sync.Mutex
	client *httpcore.Client
}

// settings is the resolved, per-ref location of one secret: which exposed
// schema, and which relation inside it.
type settings struct {
	schema string
	view   string
}

// Option configures a Provider.
type Option func(*Provider)

// WithProjectURL sets the Supabase project URL, the
// https://<project-ref>.supabase.co origin shown in the dashboard. The
// /rest/v1 mount point is appended by this package, so an operator pastes what
// the dashboard gives them rather than assembling a path.
//
// A trailing slash is trimmed so joining a path onto it never produces a double
// slash. An empty URL is ignored so it falls back to SUPABASE_URL rather than
// pinning an unusable empty origin.
func WithProjectURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.projectURL = strings.TrimRight(u, "/")
		}
	}
}

// WithServiceKey sets the project's service-role (secret) key.
//
// The key is captured in a closure rather than stored in a field, for the
// reason Provider's own doc comment gives: reflection reaches an unexported
// field and no String method can stop it. An empty key is ignored so it falls
// back to SUPABASE_SERVICE_ROLE_KEY.
//
// This must not be the anon (publishable) key. A relation exposing decrypted
// secrets is granted to service_role alone, so an anon key would be refused;
// and granting it to anon would publish the whole Vault, because the anon key
// is designed to ship in browsers.
func WithServiceKey(key string) Option {
	return func(p *Provider) {
		if key != "" {
			p.serviceKey = func() string { return key }
		}
	}
}

// WithSchema sets the exposed schema holding the relation, for refs that carry
// no ?schema= option. It defaults to public, the one schema every project
// exposes out of the box.
//
// The vault schema itself is not a legal value in any useful sense: Supabase
// restricts auth, storage, realtime and vault from the exposed-schemas list, so
// naming it here produces a PostgREST PGRST106 rather than a working read. That
// is not enforced by this package, because the restriction is the backend's to
// state and its error names the schemas that ARE available.
func WithSchema(name string) Option { return func(p *Provider) { p.schema = name } }

// WithView sets the relation name read inside the schema, for refs that carry
// no ?view= option. It defaults to decrypted_secrets, the name the README's
// setup SQL creates.
//
// It is a name rather than a path: it becomes one PostgREST resource segment,
// so a value containing a slash addresses something else entirely and is
// rejected on the first Resolve.
func WithView(name string) Option { return func(p *Provider) { p.view = name } }

// WithAllowInsecure permits an http:// project URL, and nothing else: a scheme
// that is neither http nor https is still refused.
//
// It exists for the local stack. `supabase start` serves the Data API from
// http://127.0.0.1:54321, and a self-hosted install may sit behind a local
// proxy, so refusing cleartext outright would make this provider untestable
// against the vendor's own development tooling.
//
// It takes no argument deliberately. Every request carries the service-role key
// in two headers, and that key bypasses row-level security on the entire
// project, so a cleartext origin hands total access to anything on the path. An
// opt-in that cannot be switched on by a bool variable that happened to default
// to true is the safer shape for a decision of that weight.
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

// New constructs a Supabase Vault provider. Without options it reads
// SUPABASE_URL, SUPABASE_SERVICE_ROLE_KEY, SUPABASE_VAULT_SCHEMA and
// SUPABASE_VAULT_VIEW lazily at resolve time, so it is safe to register from
// init even when no credentials exist at process start.
//
// It returns no error: every failure a constructor could report here (a missing
// key, a project URL with the wrong scheme) is reported by Resolve instead,
// wrapping mamori.ErrInvalid. `mamori doctor` resolves every ref before
// deployment, so a misconfiguration still surfaces before production, and
// keeping New single-valued is what lets a blank import register the scheme.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "supabase".
func (p *Provider) Scheme() string { return scheme }

// secretNameOf returns the ref's secret name, which is the ENTIRE ref path.
//
// No dot-segment or slash handling is needed, unlike every path-addressed
// provider: the name becomes a PostgREST filter VALUE in the query string, not
// a path segment, so "a/b" and "../etc" are ordinary names that match a row or
// match nothing. Supabase Vault does not constrain a secret name's characters,
// so refusing either would refuse a name the backend accepts.
func secretNameOf(ref mamori.Ref) (string, error) {
	name := strings.TrimPrefix(ref.Path, "/")
	if name == "" {
		return "", fmt.Errorf("mamori/supabase: ref %q requires a secret name: %w", ref.Raw, mamori.ErrInvalid)
	}
	return name, nil
}

// settingsFor resolves the schema and relation for one ref.
//
// Precedence is the ref option, then the provider option, then the environment
// variable, then the default: an operator who knows providers/cloudflare-kv's
// ?namespace= rule already knows this one. The environment is read here, per
// resolve, rather than in New, so a process that learns its configuration after
// start still works.
//
// A relation name containing a slash wraps mamori.ErrInvalid rather than being
// escaped into one segment. Unlike a secret name, this is a PostgREST resource
// and a slash in it addresses a different endpoint; there is no relation whose
// real name needs one, so accepting it could only mask a typo.
func (p *Provider) settingsFor(ref mamori.Ref) (settings, error) {
	s := settings{
		schema: firstNonEmpty(ref.Opt("schema"), p.schema, os.Getenv(envSchema), defaultSchema),
		view:   firstNonEmpty(ref.Opt("view"), p.view, os.Getenv(envView), defaultView),
	}
	if strings.ContainsAny(s.view, "/\\") {
		return settings{}, fmt.Errorf("mamori/supabase: relation name %q contains a slash; it must be one PostgREST resource name: %w",
			s.view, mamori.ErrInvalid)
	}
	return s, nil
}

// clientFor returns the shared httpcore.Client, building it on first use.
//
// Building it lazily is what lets New read no environment and return no error.
// The lock is held across construction, which is safe because construction
// performs no network call.
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

	base := firstNonEmpty(p.projectURL, strings.TrimRight(os.Getenv(envURL), "/"))
	if base == "" {
		return nil, fmt.Errorf("mamori/supabase: no project URL; set %s or use WithProjectURL: %w", envURL, mamori.ErrInvalid)
	}
	if err := checkProjectURLScheme(base, p.allowInsecure); err != nil {
		return nil, err
	}

	key := p.serviceKey
	if key == nil {
		if fromEnv := os.Getenv(envServiceKey); fromEnv != "" {
			key = func() string { return fromEnv }
		}
	}
	if key == nil {
		// The message names both the option and the environment variable, and
		// never echoes a credential that IS set.
		return nil, fmt.Errorf("mamori/supabase: no service-role key; set %s or use WithServiceKey: %w",
			envServiceKey, mamori.ErrInvalid)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     base + restPrefix,
		HTTPClient:  p.httpClient,
		Auth:        apiKeyAuth(key),
		UserAgent:   userAgent,
		ErrorDetail: errorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/supabase: building the API client: %w", err)
	}
	p.client = c
	return c, nil
}

// apiKeyAuth sets the two headers a Supabase request needs, both carrying the
// same key.
//
// Supabase's gateway authenticates the apikey header and routes the request;
// PostgREST reads the Authorization bearer JWT to decide which database role
// the query runs as. Sending only one is a documented way to get a confusing
// failure rather than a clean one: apikey alone reaches PostgREST as the
// anonymous role regardless of which key it holds, so a correct service-role
// key would still be refused the relation.
//
// The key is reached through a function rather than captured as a value here so
// that it stays inside the closure it was created in, never landing in a
// readable field of any struct this package builds.
func apiKeyAuth(key func() string) httpcore.Authenticator {
	return httpcore.AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		k := key()
		req.Header.Set("apikey", k)
		req.Header.Set("Authorization", "Bearer "+k)
		return nil
	})
}

// checkProjectURLScheme rejects a project URL this package will not send the
// service-role key to.
//
// The scheme is checked against a closed set rather than only rejecting
// http://, for the reason providers/https gives for the same check: httpcore.New
// requires only a scheme and a host, so an ftp:// typo or a ws:// paste
// constructs cleanly and then fails on every single resolve with net/http's
// "unsupported protocol scheme". Naming the closed set turns that into one
// legible error.
func checkProjectURLScheme(base string, allowInsecure bool) error {
	u, err := url.Parse(base)
	if err != nil {
		// The unparsed string is not echoed: a URL can carry userinfo.
		return fmt.Errorf("mamori/supabase: project URL is not a URL: %w: %w", mamori.ErrInvalid, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return fmt.Errorf("mamori/supabase: project URL %s is http://, which sends the service-role key in cleartext; use WithAllowInsecure to accept that: %w",
				redactURL(u), mamori.ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("mamori/supabase: project URL scheme %q is not http or https: %w", u.Scheme, mamori.ErrInvalid)
	}
}

// redactURL renders a URL for an error message with its userinfo and query
// stripped, because a project URL an operator pasted may carry either and this
// package must not put a credential into an error.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// errorDetail is httpcore's Config.ErrorDetail hook: it lifts PostgREST's
// "message" and "code" out of a failing response so an operator sees WHY a
// request was refused rather than only its status.
//
// PostgREST's error envelope has four fields, and only two of them are
// surfaced. That split is the whole point of the hook being a hook:
//
//   - "message" and "code" name the fault. PGRST106 with "The schema must be
//     one of the following" is precisely the error an operator who skipped the
//     setup will hit, and it is useless without its text.
//   - "details" is NOT surfaced. PostgREST documents it as carrying the
//     offending row, rendered as "Failing row contains (...)", and for this
//     provider a row IS the decrypted secret.
//   - "hint" is NOT surfaced either. It is free-form text Postgres composes
//     from the failing statement, with no vendor guarantee bounding what it
//     can quote, and the two selected fields already say what went wrong.
//
// A body that is not JSON, or an envelope with neither field, yields "", which
// suppresses the detail for that one response rather than falling back to
// embedding the body.
func errorDetail(_ int, body []byte) string {
	var env struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	switch {
	case env.Message != "" && env.Code != "":
		return env.Message + " (" + env.Code + ")"
	case env.Message != "":
		return env.Message
	default:
		return env.Code
	}
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
