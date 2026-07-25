// Package doppler implements a mamori provider for Doppler
// (https://www.doppler.com), the SecretOps platform.
//
// Doppler has no official Go SDK, so this provider talks to the Doppler REST
// API (https://api.doppler.com) directly with net/http. A single secret is
// fetched per resolve using the "config/secret" endpoint.
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
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/xavidop/mamori"
)

// defaultBaseURL is the Doppler REST API root.
const defaultBaseURL = "https://api.doppler.com"

// scheme is the URL scheme this provider handles.
const scheme = "doppler"

// Provider resolves doppler:// refs against the Doppler REST API. It is safe for
// concurrent use.
type Provider struct {
	token      string
	baseURL    string
	httpClient *http.Client
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
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
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

	endpoint := p.baseURL + "/v3/configs/config/secret"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return mamori.Value{}, err
	}
	q := url.Values{}
	q.Set("project", project)
	q.Set("config", config)
	q.Set("name", name)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return mamori.Value{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// handled below
	case http.StatusNotFound:
		return mamori.Value{}, fmt.Errorf("mamori/doppler: secret %q not found in %s/%s: %w", name, project, config, mamori.ErrNotFound)
	default:
		// Read a bounded amount of the error body for diagnostics. Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := fmt.Errorf("mamori/doppler: unexpected status %d fetching secret %q: %s", resp.StatusCode, name, strings.TrimSpace(string(msg)))
		return mamori.Value{}, classifyDopplerStatus(resp.StatusCode, statusErr)
	}

	var sr secretResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
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

// classifyDopplerStatus maps a Doppler REST API HTTP status onto a mamori
// classification sentinel, wrapping statusErr (the error Resolve already
// built from the response, carrying the status code and body for
// diagnostics) so both the sentinel and that context survive in the
// errors.Is chain. 404 is handled by its own branch in Resolve above and
// never reaches this function.
//
// 429 is the confirmed case: Doppler's API reference documents rate limiting
// explicitly, including the response headers that accompany a 429. 401 (a
// missing or malformed token) is well established from Doppler's
// token-based auth model and its own community support threads, though the
// API reference's status-code section only describes the 2xx/4xx/5xx
// categories in general terms rather than spelling out 401 by name. 403,
// 400, and 5xx are mapped on ordinary HTTP semantics - defensible mappings,
// not individually confirmed by Doppler's documentation. Codes not listed
// above report unknown rather than being guessed at.
func classifyDopplerStatus(code int, statusErr error) error {
	if statusErr == nil {
		return nil
	}
	var sentinel error
	switch {
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	default:
		return statusErr
	}
	return fmt.Errorf("%w: %w", sentinel, statusErr)
}
