// Package doppler implements a mamori provider for Doppler
// (https://www.doppler.com), the SecretOps platform.
//
// Doppler has no official Go SDK, so this provider talks to the Doppler REST
// API (https://api.doppler.com) directly, over
// github.com/xavidop/mamori/providers/httpcore and the standard library only,
// inheriting request building, status classification, body bounding and URL
// redaction from one shared place. A single secret is fetched per resolve
// using the "config/secret" endpoint.
//
// # Scheme
//
//	doppler://<project>/<config>#<SECRET_NAME>
//
// The URL fragment (#SECRET_NAME) names the secret to fetch and is required.
// The path carries the Doppler project and config:
//
//	APIKey string `source:"doppler://backend/prd#STRIPE_API_KEY"`
//
// # Authentication
//
// The provider authenticates with a Doppler token supplied either explicitly
// via WithToken or, when unset, from the DOPPLER_TOKEN environment variable read
// lazily at resolve time. Both personal tokens and (more commonly) service
// tokens scoped to a single config are accepted.
//
// Doppler exposes no per-secret revision identifier, so Value.Version is a
// content hash (mamori.VersionHash), which still gives mamori cheap, correct
// change detection. Resolved values are marked Sensitive.
//
// Doppler has no native change-notification API, so this provider is not
// watchable; mamori wraps it in its polling adapter automatically.
package doppler

import (
	"context"
	"encoding/json"
	"errors"
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

// defaultBaseURL is the Doppler REST API root.
const defaultBaseURL = "https://api.doppler.com"

// scheme is the URL scheme this provider handles.
const scheme = "doppler"

// secretPath is the single-secret read endpoint, joined onto the base URL.
const secretPath = "/v3/configs/config/secret"

// errBodyLimit bounds how much of a failing response body reaches an error
// string, so a hostile or broken upstream cannot put an unbounded response
// into one. httpcore hands errorDetail at most Config.MaxBody bytes and always
// drains the rest, so the connection-reuse discipline this provider used to
// hand-roll on every branch now comes from one place; this constant is only
// about how much of that body is worth quoting.
const errBodyLimit = 4096

// Provider resolves doppler:// refs against the Doppler REST API. It is safe for
// concurrent use.
type Provider struct {
	token      string
	baseURL    string
	httpClient *http.Client

	mu     sync.Mutex
	closed bool
}

// Option configures a Provider.
type Option func(*Provider)

// WithToken sets the Doppler API token (personal or service token) explicitly.
// When unset, the provider reads DOPPLER_TOKEN from the environment at resolve
// time.
func WithToken(token string) Option {
	return func(p *Provider) { p.token = token }
}

// WithBaseURL overrides the Doppler API base URL. It is primarily useful for
// tests pointing at an httptest.Server, or for a self-hosted proxy.
func WithBaseURL(baseURL string) Option {
	return func(p *Provider) {
		if baseURL != "" {
			p.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithHTTPClient injects a custom *http.Client (timeouts, transport, test
// server client). If c is nil the option is a no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a Doppler provider. Without options it targets the public
// Doppler API and reads DOPPLER_TOKEN lazily at resolve time, so it is safe to
// register from init even when no token is present at process start.
//
// Users who need explicit configuration call
// mamori.WithProvider(doppler.New(doppler.WithToken("dp.st...."))).
func New(opts ...Option) *Provider {
	p := &Provider{
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "doppler".
func (p *Provider) Scheme() string { return scheme }

// secretResponse mirrors the JSON returned by GET /v3/configs/config/secret.
type secretResponse struct {
	Name  string `json:"name"`
	Value struct {
		Raw      string `json:"raw"`
		Computed string `json:"computed"`
	} `json:"value"`
}

// Resolve fetches a single secret named by ref.Key from the project/config
// encoded in ref.Path. A 404 (or missing secret) is reported as ErrNotFound.
//
// A ref path containing a "." or ".." segment cannot escape the base URL:
// httpcore.Client.Do rejects one for every provider built on it. Nothing here
// interpolates ref.Path into a path anyway (the project and config travel as
// query parameters), but the guarantee is inherited rather than re-stated.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return mamori.Value{}, fmt.Errorf("%w: doppler: provider is closed", mamori.ErrUnavailable)
	}

	project, config, err := parsePath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	name := ref.Key
	if name == "" {
		return mamori.Value{}, fmt.Errorf("mamori/doppler: ref %q requires a #SECRET_NAME fragment", ref.Raw)
	}

	token := p.resolveToken()
	if token == "" {
		return mamori.Value{}, errors.New("mamori/doppler: no token; set DOPPLER_TOKEN or use doppler.WithToken")
	}

	client, err := p.clientFor(token)
	if err != nil {
		return mamori.Value{}, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path: secretPath,
		Query: url.Values{
			"project": {project},
			"config":  {config},
			"name":    {name},
		},
		Header: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		// A 404 is the only status with a message of its own: it names the
		// project and config the secret was looked for in, which the status
		// alone does not say. One %w, because the sentinel is already the
		// classification.
		if errors.Is(err, mamori.ErrNotFound) {
			return mamori.Value{}, fmt.Errorf("mamori/doppler: secret %q not found in %s/%s: %w", name, project, config, mamori.ErrNotFound)
		}
		// One %w: the cause already carries the sentinel httpcore classified it
		// with, and adding a second would replace that kind rather than
		// duplicate it.
		return mamori.Value{}, fmt.Errorf("mamori/doppler: fetching secret %q: %w", name, err)
	}

	var sr secretResponse
	if err := json.Unmarshal(resp.Body, &sr); err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/doppler: decoding secret %q: %w", name, err)
	}

	// Prefer the computed value (references resolved); fall back to raw.
	val := sr.Value.Computed
	if val == "" {
		val = sr.Value.Raw
	}
	b := []byte(val)

	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: true,
		Metadata: map[string]string{
			"project": project,
			"config":  config,
			"name":    name,
		},
	}, nil
}

// clientFor builds the httpcore client for one resolve.
//
// It is built per call rather than cached because the token is read lazily
// from the environment on every resolve (see resolveToken): caching the client
// would pin whichever token happened to be set on the first one. Construction
// performs no network call and reuses the provider's *http.Client, so the
// connection pool is shared across resolves regardless.
func (p *Provider) clientFor(token string) (*httpcore.Client, error) {
	c, err := httpcore.New(httpcore.Config{
		BaseURL:     p.baseURL,
		HTTPClient:  p.httpClient,
		Auth:        httpcore.Bearer(token),
		ErrorDetail: errorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/doppler: building the API client: %w", err)
	}
	return c, nil
}

// errorDetail is httpcore's Config.ErrorDetail hook: it quotes a bounded
// prefix of a failing response's body into the error message.
//
// Embedding the body verbatim, bounded, is a deliberate house convention
// shared with providers/cloudflare-kv, providers/vercel-gc and
// providers/scaleway-sm: the verbatim text is what actually tells an operator
// what the upstream rejected the request for, and the bound is what stops a
// hostile or broken upstream turning that diagnostic into an unbounded string.
// It is safe here because httpcore calls this only for a status it has already
// decided is a failure, never for the 200 whose body IS the secret.
//
// A 404 yields no detail: Resolve replaces that status with its own message
// naming the project and config, so anything returned here would be discarded.
func errorDetail(status int, body []byte) string {
	if status == http.StatusNotFound {
		return ""
	}
	if len(body) > errBodyLimit {
		body = body[:errBodyLimit]
	}
	return strings.TrimSpace(string(body))
}

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, without contacting Doppler.
//
// A client supplied through WithHTTPClient is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the caller's
// own use of that client is unaffected by closing this provider.
//
// CloseIdleConnections is skipped when the tracked client's Transport is nil.
// New's own default (unless overridden by WithHTTPClient) is exactly that
// shape - &http.Client{Timeout: ...} with no Transport set - and net/http
// resolves a nil Transport to the process-global http.DefaultTransport.
// Calling CloseIdleConnections on that client would evict idle connections
// belonging to whatever OTHER code in this process also leaves its Transport
// unset (anything built on http.DefaultClient), not just this provider's own
// traffic, so the guard fires on an ordinary, never-injected Provider too.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.httpClient != nil && p.httpClient.Transport != nil {
		p.httpClient.CloseIdleConnections()
	}
	return nil
}

// resolveToken returns the configured token, or DOPPLER_TOKEN read lazily.
func (p *Provider) resolveToken() string {
	if p.token != "" {
		return p.token
	}
	return os.Getenv("DOPPLER_TOKEN")
}

// parsePath splits "<project>/<config>" into its two required, non-empty
// segments.
func parsePath(path string) (project, config string, err error) {
	trimmed := strings.Trim(path, "/")
	segs := strings.Split(trimmed, "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", "", fmt.Errorf("mamori/doppler: path %q must be <project>/<config>", path)
	}
	return segs[0], segs[1], nil
}
