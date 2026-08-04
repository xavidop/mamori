// Package nacos implements a mamori provider for the Alibaba Nacos
// configuration service.
//
// The scheme is "nacos" and the ref grammar is:
//
//	nacos://[<group>/]<dataId>[#json-key]
//
// A Nacos configuration is addressed by three coordinates: namespace (called
// "tenant" on the wire), group, and dataId. Only the last two are in the ref.
// The namespace is on the Provider, because it is the boundary a set of
// credentials is issued against - one server address, one namespace, one login -
// exactly as a Consul token or a Kubernetes namespace scopes its provider. A ref
// that could name any namespace would make every struct tag able to reach
// another tenant's configuration with credentials that happen to span both.
//
//	LogLevel   string `source:"nacos://app.properties#log.level"`
//	DBPassword string `source:"nacos://prod/db.json#password"`
//
// With no group segment the ref uses the Provider's group, which defaults to
// Nacos's own DEFAULT_GROUP. A path with more than two segments is rejected with
// mamori.ErrInvalid rather than guessed at: dataIds conventionally use dots
// ("app.properties", "com.example.svc.yaml"), so a third slash is a mistake and
// silently folding it into the dataId would resolve a ref nobody wrote.
//
// # A raw body, not an envelope
//
// Nacos's v1 read endpoint answers with the configuration content itself as the
// response body, with no JSON wrapper. That is unusual among mamori's HTTP-backed
// providers, which nearly all unwrap a {"data": ...} envelope, and it is the
// reason this provider does no decoding at all: the bytes on the wire ARE the
// value. A #json-key fragment then selects a field out of them with
// mamori.SelectKey, exactly as elsewhere.
//
// # Native watch
//
// Nacos publishes a long-poll listener endpoint, so this provider implements
// mamori.WatchableProvider rather than being wrapped in mamori's polling
// adapter. See Watch.
//
// # Sensitivity
//
// Nacos holds application configuration rather than managed secrets, so resolved
// values are not marked Sensitive. Teams that do keep credentials in Nacos should
// wrap those fields in secret.String to get redaction anyway.
package nacos

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the URL scheme this provider handles.
const scheme = "nacos"

// defaultServerAddr is the address Nacos listens on out of the box. It is
// cleartext http because that is what a stock Nacos server speaks; see the
// module README for what that costs when credentials are in play.
const defaultServerAddr = "http://127.0.0.1:8848"

// defaultContextPath is the servlet context Nacos is deployed under. It is
// configurable on the server (server.servlet.contextPath), which is why it is an
// option here rather than baked into the endpoint paths.
const defaultContextPath = "/nacos"

// defaultGroup is Nacos's own default group name. A configuration published
// through the console with the group field left alone lands here.
const defaultGroup = "DEFAULT_GROUP"

// defaultHold is how long the listener endpoint is asked to hold one poll open.
// Nacos's Open API documents 30s for this endpoint ("The timeout for long
// polling is 30s. Enter 30,000 here"), and the server subtracts its own delay
// before parking the request.
const defaultHold = 30 * time.Second

// Provider resolves nacos:// refs against one Nacos server, in one namespace. It
// is safe for concurrent use.
//
// The HTTP client is built lazily on first Resolve or Watch, from the ambient
// NACOS_* environment unless options override it, so New never fails and never
// contacts the server. That is what lets init register a usable provider for
// `import _` wiring.
type Provider struct {
	serverAddr  string
	contextPath string
	namespace   string
	group       string
	username    string
	password    string
	httpClient  *http.Client
	auth        httpcore.Authenticator
	hold        time.Duration
	maxBody     int64

	mu     sync.Mutex
	client *httpcore.Client
	// buildErr is sticky. A bad server address does not become good on the
	// next resolve, and rebuilding the client on every call to re-derive the
	// same failure would hide the fact that it is a configuration error.
	buildErr error
	// builds counts how many times the client construction path has run. It
	// makes the build-exactly-once invariant assertable, which nothing else
	// does: a client rebuilt on every resolve produces the identical value and
	// the identical error, so the difference is invisible from the outside and
	// a test written without this cannot tell the two apart.
	builds int
}

// Option configures a Provider.
type Option func(*Provider)

// WithServerAddr sets the Nacos server base URL, e.g. "http://nacos.svc:8848"
// (default: NACOS_SERVER_ADDR, else http://127.0.0.1:8848).
//
// It is a URL rather than a host:port because the scheme decides whether
// credentials and configuration cross the network in cleartext, and that must be
// something an operator states rather than something this package assumes.
func WithServerAddr(addr string) Option {
	return func(p *Provider) { p.serverAddr = addr }
}

// WithContextPath sets the servlet context path Nacos is deployed under
// (default: NACOS_CONTEXT_PATH, else "/nacos").
//
// It exists because Nacos can be deployed behind an ingress that rewrites or
// strips the prefix; a provider that hard-coded "/nacos" would be unusable
// there, and the failure would be a 404 that looks exactly like a missing
// configuration.
func WithContextPath(path string) Option {
	return func(p *Provider) { p.contextPath = path }
}

// WithNamespace sets the namespace (the "tenant" query parameter) every ref is
// resolved in (default: NACOS_NAMESPACE, else empty, which Nacos treats as the
// "public" namespace).
func WithNamespace(id string) Option {
	return func(p *Provider) { p.namespace = id }
}

// WithGroup sets the group used by refs that do not name one (default:
// NACOS_GROUP, else DEFAULT_GROUP).
func WithGroup(group string) Option {
	return func(p *Provider) { p.group = group }
}

// WithCredentials sets the username and password used to obtain an accessToken
// from Nacos's login endpoint (default: NACOS_USERNAME / NACOS_PASSWORD).
//
// Supplying only one of the two is a configuration error and is reported on the
// first resolve, not silently ignored: a provider that quietly fell back to
// unauthenticated requests would work against a server with auth disabled and
// fail with an opaque 403 against every other one.
func WithCredentials(username, password string) Option {
	return func(p *Provider) { p.username, p.password = username, password }
}

// WithAuth injects an arbitrary httpcore.Authenticator, bypassing the
// username/password login.
//
// It is the seam for the Nacos authentication modes this provider does not
// implement natively. Nacos's pluggable auth accepts, besides the built-in
// username/password scheme, an accessKey/secretKey signature (what Alibaba
// Cloud's hosted MSE Nacos issues) and a server identity header for
// server-to-server calls. Both are header injection, which is precisely what an
// httpcore.Authenticator is, so an operator on either can pass
// httpcore.HeaderAuth or their own implementation rather than waiting for this
// package to grow a mode it cannot test against a real server.
//
// It takes precedence over WithCredentials.
func WithAuth(a httpcore.Authenticator) Option {
	return func(p *Provider) { p.auth = a }
}

// WithHTTPClient injects the http.Client used for every request. Use it to
// control transport, proxy, or TLS.
//
// The client's own Timeout must be zero or comfortably longer than the long-poll
// hold (see WithLongPollTimeout): http.Client.Timeout applies to the whole round
// trip, so a 30s client timeout against a 30s hold aborts every idle listener
// poll and turns a healthy watch into a stream of errors.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.httpClient = c }
}

// WithLongPollTimeout sets how long the listener endpoint is asked to hold one
// poll open (default: 30s, which is what the Nacos Open API documents).
//
// It changes how many requests an idle watch makes, not how quickly a change is
// seen: the server answers the instant a configuration moves, whatever the hold.
// Lowering it is mainly useful against a proxy with a short idle timeout of its
// own.
func WithLongPollTimeout(d time.Duration) Option {
	return func(p *Provider) { p.hold = d }
}

// WithMaxBody caps how many response bytes are read (default:
// httpcore.DefaultMaxBody, 1 MiB).
//
// Nacos's own default ceiling for a configuration is 100 KiB, so the default
// here already has room; raise it only for a server whose limit was raised.
func WithMaxBody(n int64) Option {
	return func(p *Provider) { p.maxBody = n }
}

// withClient injects a pre-built httpcore.Client. Unexported: it is how tests
// supply a client over an in-process http.RoundTripper.
func withClient(c *httpcore.Client) Option {
	return func(p *Provider) { p.client = c }
}

// New constructs a Nacos provider. It never fails and never contacts the server:
// the HTTP client is built on first use, so a misconfigured address surfaces as
// a classified resolve error rather than a panic in an init function.
func New(opts ...Option) *Provider {
	p := &Provider{hold: defaultHold}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient NACOS_* configuration. Callers that need explicit configuration use
// mamori.WithProvider(nacos.New(nacos.WithServerAddr("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "nacos".
func (p *Provider) Scheme() string { return scheme }

// core returns the shared httpcore.Client, building it on first use from the
// ambient environment overlaid with whatever options were supplied.
func (p *Provider) core() (*httpcore.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	if p.buildErr != nil {
		return nil, p.buildErr
	}
	p.builds++

	addr := firstNonEmpty(p.serverAddr, os.Getenv("NACOS_SERVER_ADDR"), defaultServerAddr)
	ctxPath := firstNonEmpty(p.contextPath, os.Getenv("NACOS_CONTEXT_PATH"), defaultContextPath)

	auth := p.auth
	if auth == nil {
		user := firstNonEmpty(p.username, os.Getenv("NACOS_USERNAME"))
		pass := firstNonEmpty(p.password, os.Getenv("NACOS_PASSWORD"))
		switch {
		case user != "" && pass != "":
			a, err := newTokenAuth(baseURL(addr, ctxPath), user, pass, p.httpClient)
			if err != nil {
				p.buildErr = err
				return nil, err
			}
			auth = a
		case user != "" || pass != "":
			p.buildErr = fmt.Errorf("nacos: username and password must be set together, only one was supplied: %w", mamori.ErrInvalid)
			return nil, p.buildErr
		}
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:    baseURL(addr, ctxPath),
		HTTPClient: p.watchSafeClient(),
		Auth:       auth,
		MaxBody:    p.maxBody,
		UserAgent:  "mamori-nacos",
		// ErrorDetail is deliberately nil. Nacos's v1 endpoints answer a
		// failure with a bare text body, and on a 200 that same body IS the
		// configuration, so there is no envelope field this package could
		// select and be sure it is not the value. httpcore's default, the
		// status alone, is the safe answer.
	})
	if err != nil {
		p.buildErr = fmt.Errorf("nacos: server address %q: %w", addr, err)
		return nil, p.buildErr
	}
	p.client = c
	return p.client, nil
}

// watchSafeClient returns the http.Client to use, supplying one whose Timeout
// outlasts a long-poll round when the caller gave none.
//
// httpcore's own default client has a 30s Timeout, and http.Client.Timeout
// covers the entire round trip, so leaving it in place against a 30s listener
// hold would abort every idle poll at almost exactly the moment the server was
// about to answer "nothing changed". The watch would then report a timeout
// error several times a minute against a completely healthy Nacos, which is
// worse than a broken watch because it looks like a broken backend.
//
// The margin over httpcore.LongPoll's own hold+grace budget is what keeps the
// per-round context deadline the thing that actually fires. Two deadlines racing
// at the same instant produce whichever error won, and one of them renders as a
// bare "Client.Timeout exceeded" with no context attribution.
func (p *Provider) watchSafeClient() *http.Client {
	if p.httpClient != nil {
		return p.httpClient
	}
	hold := p.hold
	if hold <= 0 {
		hold = defaultHold
	}
	return &http.Client{Timeout: hold + httpcore.DefaultLongPollGrace + 5*time.Second}
}

// baseURL joins the server address and the servlet context path.
func baseURL(addr, contextPath string) string {
	return strings.TrimSuffix(addr, "/") + "/" + strings.Trim(contextPath, "/")
}

// coordinates is one configuration's Nacos address: a group and a dataId. The
// namespace is not here because it belongs to the Provider, not the ref.
type coordinates struct {
	group  string
	dataID string
}

// coordinatesFor turns a ref into Nacos coordinates, applying the Provider's
// default group when the ref names none.
//
// It rejects an empty dataId and a path of more than two segments with
// mamori.ErrInvalid, so `mamori doctor` catches a malformed ref before
// deployment rather than letting it resolve to something nobody wrote.
func (p *Provider) coordinatesFor(ref mamori.Ref) (coordinates, error) {
	// Only a LEADING slash is trimmed. Trimming a trailing one too would make
	// "nacos://prod/" mean the dataId "prod" in the default group, when what it
	// says is the group "prod" with no dataId at all - a ref quietly resolving
	// to something other than what it names, which is the failure the whole
	// grammar check exists to prevent.
	path := strings.TrimPrefix(ref.Path, "/")
	if path == "" {
		return coordinates{}, fmt.Errorf("nacos: ref %q names no dataId: %w", ref.Raw, mamori.ErrInvalid)
	}
	group := firstNonEmpty(p.group, os.Getenv("NACOS_GROUP"), defaultGroup)
	dataID := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		group, dataID = path[:i], path[i+1:]
		if strings.Contains(dataID, "/") {
			return coordinates{}, fmt.Errorf("nacos: ref %q has more than two path segments; the grammar is nacos://[group/]dataId: %w", ref.Raw, mamori.ErrInvalid)
		}
	}
	if dataID == "" {
		return coordinates{}, fmt.Errorf("nacos: ref %q names no dataId: %w", ref.Raw, mamori.ErrInvalid)
	}
	if group == "" {
		return coordinates{}, fmt.Errorf("nacos: ref %q names an empty group: %w", ref.Raw, mamori.ErrInvalid)
	}
	return coordinates{group: group, dataID: dataID}, nil
}

// tenant returns the namespace sent as the "tenant" query parameter. Empty means
// Nacos's public namespace, which is what omitting the parameter selects.
func (p *Provider) tenant() string {
	return firstNonEmpty(p.namespace, os.Getenv("NACOS_NAMESPACE"))
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
