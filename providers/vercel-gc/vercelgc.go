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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	// Registration (mamori.Register) is deferred to the task that adds Resolve,
	// since *Provider does not satisfy mamori.Provider until then. The import is
	// kept, blank, so go.mod's require stays direct rather than being pruned by
	// `go mod tidy` and re-added later.
	_ "github.com/xavidop/mamori"
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
// is ignored when a connection string supplies its own host.
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

// Registration via mamori.Register(New()) is deferred to the task that adds
// Resolve: *Provider does not satisfy mamori.Provider until then, and an
// init() call here would fail to compile in the meantime.

// Scheme returns "vercel-gc".
func (p *Provider) Scheme() string { return scheme }

// connection resolves the effective connection, reading the environment lazily.
// Precedence: WithConnectionString, then explicit WithStoreID/WithToken, then
// GLOBAL_CONFIG, then EDGE_CONFIG.
func (p *Provider) connection() (connection, error) {
	if p.connStr != "" {
		return p.applyBaseURL(parseConnectionString(p.connStr))
	}
	if p.storeID != "" || p.token != "" {
		host := defaultHost
		if p.baseURL != "" {
			host = p.baseURL
		}
		return connection{host: host, storeID: p.storeID, token: p.token}, nil
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
		return connection{}, fmt.Errorf("mamori/vercel-gc: parsing connection string: %w", err)
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

// jsonRaw is a stored Global Config value, kept as raw JSON so no numeric
// precision is lost on the way through.
type jsonRaw = json.RawMessage
