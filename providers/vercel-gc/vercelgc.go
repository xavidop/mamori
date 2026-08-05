// Package vercelgc implements a mamori provider for Vercel Global Config
// (https://vercel.com/docs/global-config), the globally replicated key-value
// store Vercel applications read at runtime for feature flags, redirect maps,
// and experimentation settings.
//
// Vercel publishes no Go SDK, and none is needed: the read path is a
// documented HTTPS API, so this provider uses net/http and the standard
// library only.
//
// # Scheme
//
//	vercel-gc://<key>              store and token from the connection string
//	vercel-gc://<store-id>/<key>   explicit store
//	vercel-gc://<key>#field        select a field of a JSON-valued key
//
// # Authentication
//
// Connecting a Global Config store to a Vercel project sets GLOBAL_CONFIG to a
// connection string of the form
//
//	https://global-config.vercel.com/<store-id>?token=<read-token>
//
// Stores connected before Vercel renamed Edge Config to Global Config instead
// set EDGE_CONFIG, pointing at edge-config.vercel.com. This provider reads
// GLOBAL_CONFIG first and falls back to EDGE_CONFIG, and takes the host from
// the connection string rather than hardcoding it, so both keep working.
//
// # Watching
//
// Vercel exposes no streaming or blocking read for Global Config, so this
// provider deliberately does not implement mamori.WatchableProvider and mamori
// wraps it in the polling adapter. Each Resolve requests the store digest,
// which is replaced whenever the store is updated, and refetches the item body
// only when that hash moved.
package vercelgc

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "vercel-gc"

// defaultHost is used when a store id and token are supplied explicitly rather
// than through a connection string that carries its own host.
const defaultHost = "https://global-config.vercel.com"

// connection is everything needed to address one Global Config store: the API
// origin, the store id, and the read token.
type connection struct {
	host    string
	storeID string
	token   string
}

// Provider resolves vercel-gc:// refs against the Global Config read API. It is
// safe for concurrent use.
type Provider struct {
	connStr string
	storeID string
	token   string
	baseURL string

	httpClient *http.Client

	mu        sync.Mutex
	snapshots map[string]*snapshot
	closed    bool
}

// snapshot is the last observed body of one store, tagged with the digest that
// was current when it was fetched.
type snapshot struct {
	digest string
	items  map[string]jsonRaw
}

// Option configures a Provider.
type Option func(*Provider)

// WithConnectionString sets the store, token, and host from a Global Config
// connection string, overriding both GLOBAL_CONFIG and EDGE_CONFIG.
func WithConnectionString(s string) Option { return func(p *Provider) { p.connStr = s } }

// WithStoreID sets the store used by refs that name only a key.
func WithStoreID(id string) Option { return func(p *Provider) { p.storeID = id } }

// WithToken sets the Global Config read token.
func WithToken(t string) Option { return func(p *Provider) { p.token = t } }

// WithBaseURL overrides the API origin, for an httptest.Server or a proxy. It
// redirects a connection-string-derived host rather than being ignored when
// one is supplied: every request, including the Authorization header carrying
// the real token, goes to whatever host this names.
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

// New constructs a Global Config provider. Without options it reads
// GLOBAL_CONFIG (falling back to EDGE_CONFIG) lazily at resolve time, so it is
// safe to register from init even when no credentials exist at process start.
func New(opts ...Option) *Provider {
	p := &Provider{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		snapshots:  map[string]*snapshot{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "vercel-gc".
func (p *Provider) Scheme() string { return scheme }

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, through the same closed check
// clientFor already applies, without contacting Vercel.
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

// connection resolves the effective connection, reading the environment lazily.
// Precedence: WithConnectionString, then explicit WithStoreID/WithToken, then
// GLOBAL_CONFIG, then EDGE_CONFIG.
func (p *Provider) connection() (connection, error) {
	if p.connStr != "" {
		return p.applyBaseURL(parseConnectionString(p.connStr))
	}
	if p.storeID != "" || p.token != "" {
		return p.applyBaseURL(connection{host: defaultHost, storeID: p.storeID, token: p.token}, nil)
	}
	if s := os.Getenv("GLOBAL_CONFIG"); s != "" {
		return p.applyBaseURL(parseConnectionString(s))
	}
	if s := os.Getenv("EDGE_CONFIG"); s != "" {
		return p.applyBaseURL(parseConnectionString(s))
	}
	return connection{}, errors.New("mamori/vercel-gc: no connection configured; set GLOBAL_CONFIG or EDGE_CONFIG, or use WithConnectionString / WithStoreID+WithToken")
}

// applyBaseURL lets WithBaseURL redirect a connection-string-derived host,
// which is what points the provider at an httptest.Server in tests.
func (p *Provider) applyBaseURL(c connection, err error) (connection, error) {
	if err != nil {
		return connection{}, err
	}
	if p.baseURL != "" {
		c.host = p.baseURL
	}
	return c, nil
}

// parseConnectionString splits a Global Config connection string of the form
// https://<host>/<store-id>?token=<token> into its parts. The host is taken
// from the string rather than assumed, which is what keeps the legacy
// edge-config.vercel.com origin working.
func parseConnectionString(s string) (connection, error) {
	if strings.TrimSpace(s) == "" {
		return connection{}, errors.New("mamori/vercel-gc: empty connection string")
	}
	u, err := url.Parse(s)
	if err != nil {
		// url.Error's Error() is "parse \"<the whole raw URL>\": <reason>", and
		// the raw URL is s itself: the connection string, token and all. Reach
		// past it for just the reason so the returned error never carries s, or
		// any substring of it. A non-*url.Error would be unusual (url.Parse
		// documents that it always returns one), so it falls back to a generic
		// message rather than risking a type this wasn't checked against.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return connection{}, fmt.Errorf("mamori/vercel-gc: parsing connection string: %w", urlErr.Err)
		}
		return connection{}, errors.New("mamori/vercel-gc: parsing connection string: malformed URL")
	}
	storeID := strings.Trim(u.Path, "/")
	if storeID == "" || strings.Contains(storeID, "/") {
		return connection{}, errors.New("mamori/vercel-gc: connection string has no store id path segment")
	}
	token := u.Query().Get("token")
	if token == "" {
		return connection{}, errors.New("mamori/vercel-gc: connection string has no token query parameter")
	}
	if u.Scheme == "" || u.Host == "" {
		return connection{}, errors.New("mamori/vercel-gc: connection string is not an absolute URL")
	}
	return connection{host: u.Scheme + "://" + u.Host, storeID: storeID, token: token}, nil
}

// parsePath splits a ref path into a store id and a key. One segment is a key
// resolved against defaultStore; two segments are "<store-id>/<key>". This is
// unambiguous because Global Config keys are documented as alphanumeric
// characters, "_", and "-" only, so a key can never contain a slash. Charset
// validation beyond that is left to the API rather than duplicating a rule
// Vercel may loosen.
//
// Only leading slashes are trimmed before splitting. A trailing slash is
// preserved so that "<store-id>/" splits into a store segment and an empty
// key segment, reported as a missing key rather than a missing store.
func parsePath(path, defaultStore string) (store, key string, err error) {
	segs := strings.Split(strings.TrimLeft(path, "/"), "/")
	switch len(segs) {
	case 1:
		key, store = segs[0], defaultStore
	case 2:
		store, key = segs[0], segs[1]
	default:
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q has %d segments; a ref takes at most <store-id>/<key>", path, len(segs))
	}
	if key == "" {
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q requires a key", path)
	}
	if store == "" {
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q names no store and no default store is configured; set GLOBAL_CONFIG or use WithStoreID", path)
	}
	return store, key, nil
}
