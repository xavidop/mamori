// Package mamoriprov implements a mamori provider that resolves binding
// names from a mamori config server (the fan-out server in package
// github.com/xavidop/mamori/server) over its v1 HTTP wire protocol.
//
// The package is named mamoriprov, not mamori, so that code importing both
// the mamori core package and this provider does not need to alias either
// import:
//
//	import (
//		"github.com/xavidop/mamori"
//		mamoriprov "github.com/xavidop/mamori/providers/mamori"
//	)
//
// # Scheme
//
//	mamori://<binding-name>
//
// The path is a binding name registered with the upstream mamori config
// server, never an upstream ref: the server resolves the binding to its own
// upstream provider and returns only the resulting value, so the client
// cannot tell it is talking to a fan-out server.
//
// # Endpoint
//
// The server address is configured via Config.Endpoint, in one of three
// forms:
//
//	unix:///path/to.sock   dials a Unix domain socket
//	https://host:port      standard TLS, optionally with Config.TLSConfig
//	http://host:port       refused unless Config.InsecureNoTLS is true
//
// # High availability
//
// A config server running as several replicas is configured with
// Config.Endpoints, an ordered list of those same three forms, INSTEAD of
// Config.Endpoint. The order is the failover order: Resolve and ResolveBatch
// walk the list until a replica answers, and Watch rotates to the next
// endpoint on every reconnect so a dead replica cannot black-hole a watch.
// Only a failure another replica could plausibly answer differently moves on
// to the next endpoint; see Config.Endpoints for the exact rule and why an
// authoritative answer such as a permission denial is never retried.
//
// # Authentication
//
// The server authenticates inbound requests with a mamori.Authenticator,
// which is a server-side concept: it authenticates a request that has
// already arrived. The client instead needs to attach a credential to every
// outbound request, which is a different shape entirely, so this package
// does not reuse mamori.Authenticator. Use WithRequestEditor (or the
// WithHeader convenience built on it) to set an Authorization header or
// similar for BearerToken/APIKey/BasicAuth schemes. mTLS is configured via
// Config.TLSConfig.Certificates. PeerCred needs no client-side
// configuration; the kernel supplies the credential over the Unix socket.
package mamoriprov

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "mamori"

// dummyUnixHost is the fixed host used in request URLs when the endpoint is
// a Unix domain socket. The transport's DialContext ignores the address it
// is given and always dials the configured socket path, so the host in the
// URL is never actually resolved; it exists only because net/http requires
// every request URL to have one.
const dummyUnixHost = "unix"

// Config configures a Provider.
type Config struct {
	// Endpoint is the mamori config server address. See the package doc for
	// the three accepted forms (unix://, https://, http://). Point at several
	// replicas with Endpoints instead; setting both fields is a configuration
	// error.
	Endpoint string
	// Endpoints is an ordered list of mamori config server addresses, for a
	// config server run in HA mode with N replicas. Every entry takes exactly
	// the same three forms as Endpoint and is parsed by the same rules, and
	// every entry gets its OWN base URL and transport: a unix:// replica and
	// an https:// replica cannot share one, since a unix transport dials one
	// fixed socket path and ignores the request URL's host entirely. (A
	// caller-supplied HTTPClient still overrides all of them; see New.)
	//
	// Order is meaningful: it is the failover order. Resolve and ResolveBatch
	// try entries in order, moving on to the next ONLY when the current one
	// failed in a way another replica could plausibly answer differently - it
	// was unreachable, it timed out, or it reported mamori.ErrUnavailable. An
	// authoritative answer, one every replica of the same server would give
	// alike (not found, permission denied, unauthenticated, invalid, rate
	// limited), is returned immediately: see authoritativeKinds for the full
	// rule. Watch instead rotates to the next entry on every reconnect.
	//
	// Exactly one of Endpoint and Endpoints may be set. Setting both is a
	// configuration error surfaced from every call, rather than a silent
	// preference for one of them, because quietly ignoring a field hides a
	// deployment mistake: an operator adding Endpoints while an old Endpoint
	// is still set would keep talking to a single replica and never find out.
	// A parse failure in ANY entry is likewise a configuration error for the
	// whole provider, never a dropped entry, for the same reason: an operator
	// who typo'd one of three replicas must be told, not left running on two
	// while believing they have three.
	Endpoints []string
	// TLSConfig is used for https:// endpoints. Set TLSConfig.Certificates to
	// present a client certificate for mTLS. Ignored for unix:// and http://
	// endpoints.
	TLSConfig *tls.Config
	// InsecureNoTLS allows a plaintext http:// Endpoint. It is named to be
	// uncomfortable to type and read, mirroring the server's own
	// InsecureNoTLS field: both exist so an operator has to opt into
	// unencrypted traffic explicitly rather than falling into it by default.
	InsecureNoTLS bool
	// HTTPClient, when set, is used verbatim for every request to EVERY
	// endpoint, instead of the per-endpoint clients New would otherwise build
	// from each parsed endpoint's transport. See New's doc comment for the
	// precedence this implies.
	HTTPClient *http.Client
}

// endpoint is one config server replica: the base URL every request path is
// appended to, plus the HTTP client that can actually reach it. The two are
// paired rather than kept in parallel lists because a client is NOT
// interchangeable between endpoints: a unix:// endpoint's client dials one
// fixed socket path and ignores the request URL's host entirely (see
// parseEndpoint), so sending an https:// endpoint's request through it would
// quietly hit the wrong server.
type endpoint struct {
	baseURL string
	client  *http.Client
}

// Provider resolves mamori:// refs against a mamori config server. It is
// safe for concurrent use. Construct one with New.
type Provider struct {
	// endpoints is the ordered failover list built from Config.Endpoint or
	// Config.Endpoints (see newEndpoints). A single-endpoint provider is
	// simply the one-element case: nothing downstream special-cases it, so
	// there is only one code path to reason about. It is empty exactly when
	// endpointErr is set.
	endpoints      []endpoint
	requestEditors []func(*http.Request)

	// endpointErr holds any error from turning Config.Endpoint /
	// Config.Endpoints into that list: a malformed entry, a plaintext
	// endpoint without InsecureNoTLS, or both fields being set at once. New
	// never fails outright (it returns only *Provider, to match the
	// package-level registration pattern used by every provider's init
	// function), so a misconfigured endpoint is instead recorded here and
	// returned by every method (do and tryEndpoints, and therefore
	// Resolve/ResolveBatch/Watch) on first use. This makes a bad endpoint
	// fail loudly the first time the provider is actually asked to do
	// something, rather than panicking during New or silently registering a
	// provider that can never work.
	endpointErr error

	mu     sync.Mutex
	closed bool
}

// Option configures a Provider, applied after Config in New.
type Option func(*Provider)

// WithRequestEditor registers fn to run on every outbound request before it
// is sent, in registration order. This is the client-side credential
// attachment mechanism: fn typically sets an Authorization header or similar
// for BearerToken/APIKey/BasicAuth schemes. A nil fn is ignored.
func WithRequestEditor(fn func(*http.Request)) Option {
	return func(p *Provider) {
		if fn != nil {
			p.requestEditors = append(p.requestEditors, fn)
		}
	}
}

// WithHeader is a convenience for the common case of attaching one static
// header (e.g. Authorization) to every outbound request. It is implemented
// on top of WithRequestEditor.
func WithHeader(key, value string) Option {
	return WithRequestEditor(func(req *http.Request) {
		req.Header.Set(key, value)
	})
}

// WithHTTPClient injects a custom *http.Client, taking full precedence over
// the clients New would otherwise build from each parsed endpoint's
// transport; see New's doc comment. With several endpoints configured, c is
// used for ALL of them, so a caller who mixes a unix:// entry into
// Config.Endpoints must make c able to dial it. If c is nil the option is a
// no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c == nil {
			return
		}
		for i := range p.endpoints {
			p.endpoints[i].client = c
		}
	}
}

// New constructs a mamori provider from cfg, applying opts afterward.
//
// New never fails: a malformed or empty Config.Endpoint, a bad entry in
// Config.Endpoints, or both fields being set at once is recorded on the
// returned Provider and surfaced (wrapped as mamori.ErrInvalid) from the
// first call to Resolve, ResolveBatch, or Watch, rather than panicking or
// changing New's signature to return an error. This matches how a provider
// registers a zero-config default from init.
//
// HTTPClient-vs-transport precedence: if cfg.HTTPClient is set, it is used
// verbatim for every endpoint and the transports parsed from the endpoints
// (the Unix socket dialer, or the TLS config) are discarded entirely; a
// caller supplying their own client takes on full responsibility for wiring
// it correctly, including dialing a unix:// endpoint themselves if that is
// what the configuration names. Otherwise New builds one
// &http.Client{Timeout: 30 * time.Second, Transport: transport} PER endpoint
// from that endpoint's own parsed transport, with cfg.TLSConfig applied to
// each transport when set.
func New(cfg Config, opts ...Option) *Provider {
	p := &Provider{}

	endpoints, err := newEndpoints(cfg)
	if err != nil {
		p.endpointErr = err
	}
	p.endpoints = endpoints

	for _, o := range opts {
		o(p)
	}
	return p
}

// newEndpoints turns cfg's endpoint configuration into the ordered failover
// list, one entry per replica, each with its own base URL and HTTP client.
//
// It is all-or-nothing: the first entry that fails to parse aborts the whole
// list and the error becomes Provider.endpointErr, so a typo in one of three
// replicas is reported instead of leaving the caller silently running against
// two. See Config.Endpoints for why that is the right trade.
func newEndpoints(cfg Config) ([]endpoint, error) {
	addrs, err := configuredEndpoints(cfg)
	if err != nil {
		return nil, err
	}

	endpoints := make([]endpoint, 0, len(addrs))
	for _, addr := range addrs {
		baseURL, transport, err := parseEndpoint(addr, cfg.InsecureNoTLS)
		if err != nil {
			return nil, err
		}
		if cfg.TLSConfig != nil {
			transport.TLSClientConfig = cfg.TLSConfig
		}

		// Each endpoint gets its own client (and therefore its own connection
		// pool) built from its own transport, since the transports are not
		// interchangeable; see the endpoint type's doc comment. A
		// caller-supplied HTTPClient overrides all of them, per New's
		// documented precedence.
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{
				Timeout:   30 * time.Second,
				Transport: transport,
			}
		}
		endpoints = append(endpoints, endpoint{baseURL: baseURL, client: client})
	}
	return endpoints, nil
}

// configuredEndpoints returns the endpoint addresses cfg names, in failover
// order, enforcing that exactly one of Config.Endpoint and Config.Endpoints
// is set.
//
// An empty Config.Endpoint with no Config.Endpoints falls through to the
// single-element case on purpose, so the resulting "empty mamori endpoint"
// error comes from parseEndpoint exactly as it did before Endpoints existed,
// rather than from a second, differently-worded check here.
func configuredEndpoints(cfg Config) ([]string, error) {
	switch {
	case cfg.Endpoint != "" && len(cfg.Endpoints) > 0:
		return nil, fmt.Errorf("%w: Config.Endpoint (%q) and Config.Endpoints (%d entries) are both set; set exactly one", mamori.ErrInvalid, cfg.Endpoint, len(cfg.Endpoints))
	case len(cfg.Endpoints) > 0:
		return cfg.Endpoints, nil
	default:
		return []string{cfg.Endpoint}, nil
	}
}

func init() { mamori.Register(New(Config{})) }

// Scheme returns "mamori".
func (p *Provider) Scheme() string { return scheme }

// Close marks the provider closed and returns every endpoint's idle HTTP
// connections to its pool. It is idempotent, and afterwards Resolve,
// ResolveBatch and Watch all report errors.Is(err, mamori.ErrUnavailable)
// locally, through the same closed check do already applies, without
// contacting any replica.
//
// Config.HTTPClient, when supplied, is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the
// caller's own use of that client is unaffected by closing this provider.
// When it was set every endpoint shares that identical client, so this loop
// releases its idle connections once per endpoint reference rather than
// once overall, which is harmless: CloseIdleConnections only ever drops
// connections currently sitting idle in the pool. An endpoint built from its
// own parsed transport (no Config.HTTPClient) is released the same way,
// since that client belongs to this provider alone.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, ep := range p.endpoints {
		if ep.client != nil {
			ep.client.CloseIdleConnections()
		}
	}
	return nil
}

// parseEndpoint parses a Config.Endpoint into a base URL and an
// *http.Transport suitable for reaching it, per the three forms documented
// on Config.Endpoint:
//
//   - "unix:///path/to.sock": returns a transport whose DialContext always
//     dials the Unix socket at that path, and the fixed base URL
//     "http://unix" (the real path is appended per-request by the caller;
//     the host is never actually used to resolve anything since DialContext
//     ignores it).
//   - "https://host:port": returns the endpoint itself as the base URL and a
//     bare *http.Transport (the caller may attach a *tls.Config via
//     TLSClientConfig).
//   - "http://host:port": returns the endpoint as the base URL and a bare
//     transport, but ONLY when insecure is true; otherwise it returns an
//     error satisfying errors.Is(err, mamori.ErrInvalid).
//
// An empty or unparseable endpoint, or one using any other scheme, also
// returns an error satisfying errors.Is(err, mamori.ErrInvalid).
func parseEndpoint(endpoint string, insecure bool) (baseURL string, transport *http.Transport, err error) {
	if endpoint == "" {
		return "", nil, fmt.Errorf("%w: empty mamori endpoint", mamori.ErrInvalid)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", nil, fmt.Errorf("%w: parsing mamori endpoint %q: %s", mamori.ErrInvalid, endpoint, err)
	}

	switch u.Scheme {
	case "unix":
		socketPath := u.Path
		if socketPath == "" {
			return "", nil, fmt.Errorf("%w: unix mamori endpoint %q has no socket path", mamori.ErrInvalid, endpoint)
		}
		dialer := &net.Dialer{}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		return "http://" + dummyUnixHost, transport, nil

	case "https":
		return strings.TrimRight(endpoint, "/"), &http.Transport{}, nil

	case "http":
		if !insecure {
			return "", nil, fmt.Errorf("%w: plaintext mamori endpoint %q refused; set Config.InsecureNoTLS to allow it", mamori.ErrInvalid, endpoint)
		}
		return strings.TrimRight(endpoint, "/"), &http.Transport{}, nil

	default:
		return "", nil, fmt.Errorf("%w: unsupported mamori endpoint scheme %q in %q (want unix://, https://, or http://)", mamori.ErrInvalid, u.Scheme, endpoint)
	}
}

// do builds a request against ONE endpoint's base URL and path, applies any
// call-specific edits (e.g. Watch's Accept: text/event-stream header) and
// then every registered request editor, and sends it with that endpoint's
// HTTP client. Resolve, ResolveBatch and Watch all build on this. It returns
// the stored endpoint error first, if the endpoint configuration failed to
// parse during New.
//
// Which endpoint to use is the CALLER's decision, not do's: Resolve and
// ResolveBatch let tryEndpoints walk the failover list for them, and Watch
// rotates through it on reconnect (see watchLoop). Keeping that choice out of
// do is what lets one request-building path serve both policies.
//
// The endpointErr check stays here, rather than only in tryEndpoints, because
// watchLoop does not go through tryEndpoints: a misconfigured provider has no
// endpoints at all, and do must report why before it ever touches the (then
// zero-valued) endpoint it was handed.
//
// edits is variadic and almost always empty (Resolve and ResolveBatch pass
// none), so it adds no burden on those existing call sites; it exists solely
// so Watch can attach a header specific to a single call without do needing
// to know anything about SSE. edits run before p.requestEditors so a
// registered credential editor always has the final say if the two ever
// conflict.
func (p *Provider) do(ctx context.Context, ep endpoint, method, path string, body io.Reader, edits ...func(*http.Request)) (*http.Response, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("%w: mamori: provider is closed", mamori.ErrUnavailable)
	}
	if p.endpointErr != nil {
		return nil, p.endpointErr
	}

	req, err := http.NewRequestWithContext(ctx, method, ep.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: building request %s %s: %s", mamori.ErrInvalid, method, path, err)
	}
	for _, edit := range edits {
		edit(req)
	}
	for _, edit := range p.requestEditors {
		edit(req)
	}

	resp, err := ep.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
