// Package firebaserc implements a mamori provider for Firebase Remote Config.
//
// It registers the "firebase-rc" scheme. Refs take the form:
//
//	firebase-rc://<parameter-key>[#json-key]
//
// where <parameter-key> is the name of a parameter in the project's server
// Remote Config template and the optional #json-key selects a field from a
// JSON-object parameter value:
//
//	WelcomeMessage string `source:"firebase-rc://welcome_message"`
//	FeatureFlag    string `source:"firebase-rc://feature_flags#new_ui"`
//
// The provider reads the current *server* Remote Config template (the one used
// by Admin SDK / server workloads) via the Firebase Remote Config REST API,
// over github.com/xavidop/mamori/providers/httpcore, and returns the named
// parameter's default (server-side) value. Parameters that do
// not exist, or that are configured to use the in-app default (no server value),
// resolve to an error satisfying errors.Is(err, mamori.ErrNotFound).
//
// Authentication uses Application Default Credentials (ADC): the
// GOOGLE_APPLICATION_CREDENTIALS service-account key, gcloud user credentials,
// or the workload identity / metadata server on Google Cloud. The project ID is
// taken from the credentials by default and can be overridden with WithProjectID.
// The underlying HTTP client is created lazily on first use, so registration
// never fails in environments without credentials.
//
// Remote Config has no cheap native push for the server template, so this
// provider is not watchable: mamori polls it on the configured interval.
package firebaserc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// scheme is the URL scheme this provider handles.
const scheme = "firebase-rc"

// remoteConfigScope is the OAuth2 scope required to read a project's server
// Remote Config template.
const remoteConfigScope = "https://www.googleapis.com/auth/firebase.remoteconfig"

// defaultBaseURL is the Firebase Remote Config REST endpoint base.
const defaultBaseURL = "https://firebaseremoteconfig.googleapis.com/v1"

// maxBodyBytes caps how much of an API response body is read, guarding against
// an unbounded response.
const maxBodyBytes = 1 << 24 // 16 MiB

// parameter is a single Remote Config parameter's server-side value.
type parameter struct {
	// value is the concrete server-side default value.
	value string
	// hasValue is true only when a concrete server-side default value is set.
	// It is false for parameters that use the in-app default (no server value).
	hasValue bool
}

// template is the decoded subset of a server Remote Config template the provider
// needs: its parameters and a version identifier.
type template struct {
	// version is the template's version number (a monotonically increasing,
	// template-wide identifier). Empty if the backend did not supply one.
	version string
	// parameters maps parameter key to its server-side value.
	parameters map[string]parameter
}

// templateFetcher fetches the current server Remote Config template. The real
// implementation (httpFetcher) calls the Firebase Remote Config REST API; tests
// inject an in-memory fake.
type templateFetcher interface {
	fetchTemplate(ctx context.Context) (*template, error)
}

// Provider resolves firebase-rc:// refs against a project's server Remote Config
// template. It is safe for concurrent use.
type Provider struct {
	mu      sync.Mutex
	fetcher templateFetcher
	// closed records that Close ran. It is what makes Close terminal: without
	// it a closed provider keeps serving through the fetcher whose idle
	// connections were just released, as though nothing had happened.
	closed bool

	// Configuration used to build the default (REST) fetcher lazily on first use.
	projectID  string
	httpClient *http.Client
	credsJSON  []byte
	baseURL    string
}

// Option configures a Provider.
type Option func(*Provider)

// WithProjectID sets the Firebase / Google Cloud project ID whose Remote Config
// template is read. If unset, the project ID from the resolved credentials (ADC
// or a supplied service-account key) is used.
func WithProjectID(id string) Option {
	return func(p *Provider) { p.projectID = id }
}

// WithCredentialsJSON supplies a Google service-account key (the JSON file
// contents) used to authenticate. When set, ADC is not consulted. If no project
// ID has been set explicitly, the project ID embedded in the key is used.
func WithCredentialsJSON(data []byte) Option {
	return func(p *Provider) { p.credsJSON = data }
}

// WithHTTPClient injects a pre-built HTTP client used to call the Remote Config
// REST API. The client is expected to add authentication (e.g. an oauth2
// transport). This is primarily useful for tests, emulators, or custom
// transports; when set, no credentials are resolved.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.httpClient = c }
}

// WithBaseURL overrides the Remote Config REST endpoint base (default
// https://firebaseremoteconfig.googleapis.com/v1). Useful for tests and
// emulators.
func WithBaseURL(url string) Option {
	return func(p *Provider) { p.baseURL = url }
}

// WithFetcher injects a template fetcher directly, bypassing all default
// construction. Tests use it to supply an in-memory fake; advanced callers can
// use it to fully control how the template is fetched.
func WithFetcher(f templateFetcher) Option {
	return func(p *Provider) { p.fetcher = f }
}

// New constructs a Firebase Remote Config provider. By default the underlying
// HTTP client is created lazily on first Resolve using Application Default
// Credentials, so New never contacts the network and never fails for lack of
// credentials.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "firebase-rc".
func (p *Provider) Scheme() string { return scheme }

// getFetcher returns the backing template fetcher, building the default REST
// fetcher lazily (and caching it) on first use. A closed provider is refused
// here, before the cached-fetcher check, so nothing is served or built after
// Close.
func (p *Provider) getFetcher(ctx context.Context) (templateFetcher, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("firebase-rc: provider is closed: %w", mamori.ErrUnavailable)
	}
	if p.fetcher != nil {
		return p.fetcher, nil
	}
	f, err := p.buildFetcher(ctx)
	if err != nil {
		return nil, err
	}
	p.fetcher = f
	return f, nil
}

// buildFetcher constructs the default REST fetcher from the provider's
// configuration, resolving credentials via ADC (or a supplied key) unless an
// HTTP client was injected. Callers must hold p.mu.
func (p *Provider) buildFetcher(ctx context.Context) (templateFetcher, error) {
	projectID := p.projectID
	httpClient := p.httpClient

	if httpClient == nil {
		var (
			creds *google.Credentials
			err   error
		)
		if len(p.credsJSON) > 0 {
			// The credential JSON is supplied by the operator via WithCredentialsJSON
			// (their own service account), not an untrusted source, so the security
			// rationale behind this deprecation does not apply here.
			params := google.CredentialsParams{Scopes: []string{remoteConfigScope}}
			creds, err = google.CredentialsFromJSONWithParams(ctx, p.credsJSON, params) //nolint:staticcheck // operator-supplied credentials, not untrusted input
		} else {
			creds, err = google.FindDefaultCredentials(ctx, remoteConfigScope)
		}
		if err != nil {
			return nil, fmt.Errorf("firebase-rc: obtaining credentials: %w", err)
		}
		if projectID == "" {
			projectID = creds.ProjectID
		}
		httpClient = oauth2.NewClient(ctx, creds.TokenSource)
	}

	if projectID == "" {
		return nil, fmt.Errorf("firebase-rc: no project ID; set one with WithProjectID or via credentials/ADC")
	}

	baseURL := p.baseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	// MaxBody is set explicitly rather than left at httpcore's 1 MiB default:
	// a server Remote Config template is a whole project's parameter set, not
	// a single value, and this provider has always allowed 16 MiB for one.
	client, err := httpcore.New(httpcore.Config{
		BaseURL:     baseURL,
		HTTPClient:  httpClient,
		MaxBody:     maxBodyBytes,
		ErrorDetail: errorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("firebase-rc: building the Remote Config client: %w", err)
	}
	return &httpFetcher{
		projectID:  projectID,
		httpClient: httpClient,
		client:     client,
	}, nil
}

// Close is terminal: every later Resolve reports unavailable without
// contacting the Remote Config API. It is safe to call multiple times, and on
// a provider that never resolved.
//
// It also calls CloseIdleConnections on an httpClient supplied through
// WithHTTPClient, but only when that client's Transport is non-nil: a nil
// Transport is resolved by net/http to the process-global
// http.DefaultTransport, and releasing idle connections there would evict
// connections belonging to unrelated code elsewhere in the process rather
// than anything this provider used. The client itself is never invalidated
// either way - only its idle connections are ever released, so the caller's
// own use of it is unaffected by closing this provider.
//
// On the default (no WithHTTPClient) path this releases nothing at all, and
// that is not a gap this guard introduces: buildFetcher's default client
// wraps *oauth2.Transport, which implements no CloseIdleConnections method,
// so http.Client.CloseIdleConnections has always silently no-op'd on it.
// Idle connections DO exist on that path - oauth2.Transport.Base is left nil,
// so it falls through to http.DefaultTransport for the actual round trip,
// same as every unguarded default client in this tier - they are simply not
// this provider's alone to release, and there is no method to call that
// would release only them. Only an injected client can ever be released
// here.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if hf, ok := p.fetcher.(*httpFetcher); ok && hf.httpClient != nil && hf.httpClient.Transport != nil {
		hf.httpClient.CloseIdleConnections()
	}
	return nil
}

// Resolve fetches the current server Remote Config template and returns the
// value of the parameter named by ref.Path. When ref.Key is set, the JSON
// payload field is selected. Unknown parameters, and parameters that use the
// in-app default (no server value), return an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	if ref.Path == "" {
		return mamori.Value{}, fmt.Errorf("firebase-rc: ref %q must be of the form firebase-rc://<parameter-key>[#json-key]", ref.Raw)
	}

	fetcher, err := p.getFetcher(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	tmpl, err := fetcher.fetchTemplate(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	param, ok := tmpl.parameters[ref.Path]
	if !ok {
		return mamori.Value{}, fmt.Errorf("firebase-rc: parameter %q not found in server template: %w", ref.Path, mamori.ErrNotFound)
	}
	if !param.hasValue {
		return mamori.Value{}, fmt.Errorf("firebase-rc: parameter %q has no server-side value (uses in-app default): %w", ref.Path, mamori.ErrNotFound)
	}

	data := []byte(param.value)

	// Prefer the native template version number for cheap change detection; fall
	// back to a content hash when the backend supplies no version.
	ver := tmpl.version
	if ver == "" {
		ver = mamori.VersionHash(data)
	}

	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: false,
	}, nil
}

// httpFetcher fetches the server Remote Config template over the REST API.
//
// httpClient is retained alongside the httpcore client purely so Close can
// release its idle connections; every request goes through client.
type httpFetcher struct {
	projectID  string
	httpClient *http.Client
	client     *httpcore.Client
}

var _ templateFetcher = (*httpFetcher)(nil)

// fetchTemplate reads the current server template.
//
// The status is classified by httpcore.Client.Do, with one status taken back:
// see notFoundIsNotAMissingParameter below.
func (f *httpFetcher) fetchTemplate(ctx context.Context) (*template, error) {
	resp, err := f.client.Do(ctx, httpcore.Request{
		Path: "/projects/" + url.PathEscape(f.projectID) + "/remoteConfig",
	})
	if err != nil {
		return nil, notFoundIsNotAMissingParameter(err)
	}
	return decodeTemplate(resp.Body)
}

// notFoundIsNotAMissingParameter keeps an HTTP 404 from this API out of
// mamori's not-found kind.
//
// httpcore.ClassifyStatus maps 404 to mamori.ErrNotFound, which is correct for
// a provider whose request addresses one value: the value is absent. This
// request addresses a whole project's template, so a 404 means the project does
// not exist or the Remote Config API is not enabled on it - a misconfiguration
// affecting every ref at once. ErrNotFound is the one kind that makes mamori
// apply a field's default instead of failing, so letting it through would turn
// a wrong project id into every firebase-rc field silently taking its default,
// announcing nothing. A missing PARAMETER is reported as ErrNotFound by Resolve
// itself, from the decoded template, and that is the only thing that should be.
//
// The cause is rendered into the message rather than wrapped, the one place in
// this package that does so, and deliberately: errors.Is offers no way to veto
// a sentinel that is already in the chain, so keeping the chain whole here
// would keep mamori.ErrNotFound reachable with it. Everything the message
// carried - the status, the API's own diagnostic snippet - survives as text,
// and the kind goes back to being unclassified, exactly as it was before this
// provider moved onto httpcore.
func notFoundIsNotAMissingParameter(err error) error {
	if errors.Is(err, mamori.ErrNotFound) {
		return fmt.Errorf("firebase-rc: fetching Remote Config template: %s", err)
	}
	// One %w: the cause already carries the sentinel httpcore classified it
	// with, and adding a second would replace that kind rather than duplicate
	// it.
	return fmt.Errorf("firebase-rc: fetching Remote Config template: %w", err)
}

// errorDetail is httpcore's Config.ErrorDetail hook: it lifts a short prefix of
// a failing response's body into the error message, so an operator sees the
// API's own diagnostic ("PERMISSION_DENIED", "Requested entity was not found")
// rather than only a status. httpcore calls it only for a status it has
// already decided is a failure, never for the 200 whose body is the template.
//
// A Remote Config template is not secret material (parameters are served to
// clients), and the error envelope is a status plus a reason, so quoting a
// bounded prefix of it discloses nothing a resolved value would.
func errorDetail(_ int, body []byte) string { return snippet(body) }

// restTemplate is the decoded subset of the Remote Config REST response.
type restTemplate struct {
	Parameters map[string]struct {
		DefaultValue *struct {
			Value           *string `json:"value"`
			UseInAppDefault bool    `json:"useInAppDefault"`
		} `json:"defaultValue"`
	} `json:"parameters"`
	Version struct {
		VersionNumber string `json:"versionNumber"`
	} `json:"version"`
}

// decodeTemplate parses a Remote Config REST response body into a template.
func decodeTemplate(body []byte) (*template, error) {
	var rt restTemplate
	if err := json.Unmarshal(body, &rt); err != nil {
		return nil, fmt.Errorf("firebase-rc: decoding template: %w", err)
	}
	tmpl := &template{
		version:    rt.Version.VersionNumber,
		parameters: make(map[string]parameter, len(rt.Parameters)),
	}
	for key, p := range rt.Parameters {
		var param parameter
		if p.DefaultValue != nil && p.DefaultValue.Value != nil && !p.DefaultValue.UseInAppDefault {
			param.value = *p.DefaultValue.Value
			param.hasValue = true
		}
		tmpl.parameters[key] = param
	}
	return tmpl, nil
}

// snippet returns a short, printable prefix of an API error body for diagnostics.
func snippet(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
