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
// by Admin SDK / server workloads) via the Firebase Remote Config REST API and
// returns the named parameter's default (server-side) value. Parameters that do
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
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/xavidop/mamori"
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
// fetcher lazily (and caching it) on first use.
func (p *Provider) getFetcher(ctx context.Context) (templateFetcher, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	return &httpFetcher{
		projectID:  projectID,
		httpClient: httpClient,
		baseURL:    baseURL,
	}, nil
}

// Close releases idle connections held by a lazily-built HTTP client, if any.
// It is safe to call multiple times.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if hf, ok := p.fetcher.(*httpFetcher); ok && hf.httpClient != nil {
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
type httpFetcher struct {
	projectID  string
	httpClient *http.Client
	baseURL    string
}

var _ templateFetcher = (*httpFetcher)(nil)

func (f *httpFetcher) fetchTemplate(ctx context.Context) (*template, error) {
	url := fmt.Sprintf("%s/projects/%s/remoteConfig", f.baseURL, f.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("firebase-rc: building request: %w", err)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firebase-rc: fetching template: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("firebase-rc: reading template response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firebase-rc: Remote Config API returned %s: %s", resp.Status, snippet(body))
	}
	return decodeTemplate(body)
}

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
