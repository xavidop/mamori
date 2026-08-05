// Package cloudflarekv implements a mamori provider for Cloudflare Workers KV
// (https://developers.cloudflare.com/kv/), Cloudflare's low-latency,
// eventually-consistent key-value store replicated to its edge network.
//
// Cloudflare publishes a Go SDK, but it is not used here: the read path is a
// documented HTTPS API, so this provider uses net/http and the standard
// library only, keeping the SDK's dependency tree out of every consumer's
// build.
//
// # Scheme
//
//	cloudflare-kv://<key>                  key from the configured namespace
//	cloudflare-kv://<key>?namespace=<id>    explicit namespace
//
// The entire ref path is the key, including any slashes it contains. Workers
// KV keys are up to 512 bytes of any printable, non-whitespace characters, so
// a key like "config/prod/log-level" is one ordinary key name rather than a
// namespace plus a shorter key. That is why the namespace is never taken from
// the path: it comes only from configuration or the ref's ?namespace= option.
//
// # Authentication
//
// Reading a key requires an API token, an account id, and a namespace id.
// Each may be set explicitly (WithAPIToken, WithAccountID, WithNamespaceID)
// or read from the environment (CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID,
// CLOUDFLARE_KV_NAMESPACE_ID); an explicit option wins over its environment
// variable. The namespace has one more source: a ref's ?namespace= query
// option, which wins over both, letting a single provider serve refs that
// point at different namespaces.
//
// # Watching
//
// The Workers KV REST API exposes no streaming or blocking read, so this
// provider deliberately does not implement mamori.WatchableProvider and
// mamori wraps it in the polling adapter instead.
package cloudflarekv

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

const (
	scheme         = "cloudflare-kv"
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
)

// Provider resolves cloudflare-kv:// refs against the Workers KV REST API. It
// is safe for concurrent use.
type Provider struct {
	apiToken    string
	accountID   string
	namespaceID string
	baseURL     string

	httpClient *http.Client

	mu     sync.Mutex
	closed bool
}

// settings is the resolved, per-ref configuration needed to address one
// Workers KV value: the API token, the account id, and the namespace id.
type settings struct {
	token     string
	account   string
	namespace string
}

// Option configures a Provider.
type Option func(*Provider)

// WithAPIToken sets the Cloudflare API token used to authenticate requests.
func WithAPIToken(token string) Option { return func(p *Provider) { p.apiToken = token } }

// WithAccountID sets the Cloudflare account id that owns the namespace.
func WithAccountID(id string) Option { return func(p *Provider) { p.accountID = id } }

// WithNamespaceID sets the default Workers KV namespace id used by refs that
// do not carry a ?namespace= option.
func WithNamespaceID(id string) Option { return func(p *Provider) { p.namespaceID = id } }

// WithBaseURL overrides the API origin, for an httptest.Server or a proxy.
// A trailing slash is trimmed so that joining it with a path never produces
// a double slash.
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient injects a custom *http.Client. A nil client is a no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a Workers KV provider. Without options it reads
// CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID, and CLOUDFLARE_KV_NAMESPACE_ID
// lazily at resolve time, so it is safe to register from init even when no
// credentials exist at process start.
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

// Scheme returns "cloudflare-kv".
func (p *Provider) Scheme() string { return scheme }

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve and ResolveBatch report
// errors.Is(err, mamori.ErrUnavailable) locally, through the same closed check
// clientFor already applies, without contacting Workers KV.
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

// keyOf returns the ref's key. The ENTIRE ref path is the key, deliberately.
//
// Workers KV keys may contain slashes: they are up to 512 bytes of any
// printable, non-whitespace characters, so "config/prod/log-level" is one
// ordinary key name. A segment-count rule like the one providers/vercel-gc
// uses to split "<store>/<key>" would silently misread that common shape as a
// namespace plus a shorter key, so the namespace is selected with the
// ?namespace= query option instead and never taken from the path.
func keyOf(ref mamori.Ref) (string, error) {
	if ref.Path == "" {
		return "", fmt.Errorf("mamori/cloudflare-kv: ref %q requires a key", ref.Raw)
	}
	return ref.Path, nil
}

// settingsFor resolves the credentials and namespace for one ref, reading the
// environment lazily so registering from init is safe with no credentials at
// process start. Precedence is the explicit option, then the environment; the
// namespace additionally lets the ref's ?namespace= override both.
func (p *Provider) settingsFor(ref mamori.Ref) (settings, error) {
	s := settings{
		token:     firstNonEmpty(p.apiToken, os.Getenv("CLOUDFLARE_API_TOKEN")),
		account:   firstNonEmpty(p.accountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
		namespace: firstNonEmpty(ref.Opt("namespace"), p.namespaceID, os.Getenv("CLOUDFLARE_KV_NAMESPACE_ID")),
	}
	// Each message names both the option and the environment variable that
	// would supply the value, and never echoes any credential that IS set.
	switch {
	case s.token == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no API token; set CLOUDFLARE_API_TOKEN or use WithAPIToken")
	case s.account == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no account id; set CLOUDFLARE_ACCOUNT_ID or use WithAccountID")
	case s.namespace == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no namespace id; set CLOUDFLARE_KV_NAMESPACE_ID, use WithNamespaceID, or add ?namespace= to the ref")
	}
	return s, nil
}

// firstNonEmpty returns the first non-empty string among vals, or "" if all
// are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
