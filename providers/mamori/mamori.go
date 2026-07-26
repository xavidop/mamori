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
	// the three accepted forms (unix://, https://, http://).
	Endpoint string
	// TLSConfig is used for https:// endpoints. Set TLSConfig.Certificates to
	// present a client certificate for mTLS. Ignored for unix:// and http://
	// endpoints.
	TLSConfig *tls.Config
	// InsecureNoTLS allows a plaintext http:// Endpoint. It is named to be
	// uncomfortable to type and read, mirroring the server's own
	// InsecureNoTLS field: both exist so an operator has to opt into
	// unencrypted traffic explicitly rather than falling into it by default.
	InsecureNoTLS bool
	// HTTPClient, when set, is used verbatim for every request instead of the
	// client New would otherwise build from the parsed Endpoint's transport.
	// See New's doc comment for the precedence this implies.
	HTTPClient *http.Client
}

// Provider resolves mamori:// refs against a mamori config server. It is
// safe for concurrent use. Construct one with New.
type Provider struct {
	baseURL        string
	httpClient     *http.Client
	requestEditors []func(*http.Request)

	// endpointErr holds any error from parsing Config.Endpoint. New never
	// fails outright (it returns only *Provider, to match the package-level
	// registration pattern used by every provider's init function), so a
	// misconfigured endpoint is instead recorded here and returned by every
	// method (do, and therefore Resolve/ResolveBatch/Watch) on first use.
	// This makes a bad endpoint fail loudly the first time the provider is
	// actually asked to do something, rather than panicking during New or
	// silently registering a provider that can never work.
	endpointErr error
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
// the client New would otherwise build from the parsed Endpoint's transport;
// see New's doc comment. If c is nil the option is a no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a mamori provider from cfg, applying opts afterward.
//
// New never fails: a malformed or empty Config.Endpoint is recorded on the
// returned Provider and surfaced (wrapped as mamori.ErrInvalid) from the
// first call to Resolve, ResolveBatch, or Watch, rather than panicking or
// changing New's signature to return an error. This matches how a provider
// registers a zero-config default from init.
//
// HTTPClient-vs-transport precedence: if cfg.HTTPClient is set, it is used
// verbatim and the transport parsed from cfg.Endpoint (the Unix socket
// dialer, or the TLS config) is discarded entirely; a caller supplying their
// own client takes on full responsibility for wiring it correctly,
// including dialing a unix:// endpoint themselves if that is what
// Config.Endpoint names. Otherwise New builds
// &http.Client{Timeout: 30 * time.Second, Transport: transport} from the
// parsed endpoint, with cfg.TLSConfig applied to that transport when set.
func New(cfg Config, opts ...Option) *Provider {
	p := &Provider{}

	baseURL, transport, err := parseEndpoint(cfg.Endpoint, cfg.InsecureNoTLS)
	if err != nil {
		p.endpointErr = err
	}
	p.baseURL = baseURL

	if transport != nil && cfg.TLSConfig != nil {
		transport.TLSClientConfig = cfg.TLSConfig
	}

	if cfg.HTTPClient != nil {
		p.httpClient = cfg.HTTPClient
	} else {
		p.httpClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New(Config{})) }

// Scheme returns "mamori".
func (p *Provider) Scheme() string { return scheme }

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

// do builds a request against the provider's base URL and path, applies any
// call-specific edits (e.g. Watch's Accept: text/event-stream header) and
// then every registered request editor, and sends it with the provider's
// HTTP client. Tasks 2-4 (Resolve, ResolveBatch, Watch) build on this. It
// returns the stored endpoint error first, if Config.Endpoint failed to
// parse during New.
//
// edits is variadic and almost always empty (Resolve and ResolveBatch pass
// none), so it adds no burden on those existing call sites; it exists solely
// so Watch can attach a header specific to a single call without do needing
// to know anything about SSE. edits run before p.requestEditors so a
// registered credential editor always has the final say if the two ever
// conflict.
func (p *Provider) do(ctx context.Context, method, path string, body io.Reader, edits ...func(*http.Request)) (*http.Response, error) {
	if p.endpointErr != nil {
		return nil, p.endpointErr
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: building request %s %s: %s", mamori.ErrInvalid, method, path, err)
	}
	for _, edit := range edits {
		edit(req)
	}
	for _, edit := range p.requestEditors {
		edit(req)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
