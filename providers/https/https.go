// Package https implements a generic mamori provider for configuration and
// secrets served over HTTP by an endpoint you declare.
//
// # Scheme
//
//	https://<endpoint>/<path>[#<key>][?<opts>]
//
// <endpoint> is not a hostname. It is the Name of an Endpoint registered with
// [New], and a ref naming an unregistered endpoint fails with mamori.ErrInvalid
// so `mamori doctor` catches the typo before deployment.
//
// Three problems forced that design, and it solves all three.
//
// A ref cannot carry target query parameters. mamori's grammar is
// scheme://path[#key][?opts] with the fragment BEFORE the query, and ?opts is
// mamori's own namespace for decode and debounce. A ref written
// "https://api.example.com/cfg?env=prod#/db/pass" does not fail loudly: ParseRef
// splits on the first '?', so Key comes out empty and Opts comes out as
// env="prod#/db/pass". Fixed query parameters therefore live on the Endpoint.
//
// A ref cannot carry credentials, because a struct tag is source code. Auth
// lives on the Endpoint, where it can be read from the environment at startup.
//
// A provider that fetched an arbitrary URL would make every struct tag a
// potential SSRF. Restricting refs to declared endpoints matches the posture the
// rest of mamori takes: the config server serves a fixed, operator-declared
// binding table and never a client-supplied ref, and exec: is opt-in for the
// same class of reason.
//
// # Watching
//
// A generic HTTP endpoint exposes no push channel, so this provider
// deliberately does not implement mamori.WatchableProvider and mamori wraps it
// in the polling adapter. Each poll goes through httpcore.Revalidator, so it is
// a conditional GET: an unchanged value costs a 304 rather than a full payload.
package https

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the ref scheme this provider registers.
const scheme = "https"

// Endpoint is one operator-declared backend. Its Name is what a ref's authority
// selects.
type Endpoint struct {
	// Name is the ref authority, e.g. "billing" in https://billing/cfg. It must
	// be non-empty and must not contain '/', '?' or '#'. Required.
	Name string
	// BaseURL is the root every ref path is joined onto. Required. An http://
	// BaseURL is rejected unless AllowInsecure is set.
	BaseURL string
	// Auth injects credentials. Nil sends requests unauthenticated.
	Auth httpcore.Authenticator
	// Query is merged into every request to this endpoint. It exists because a
	// ref cannot carry target query parameters; see the package documentation.
	// New copies it, so mutating the map afterwards does not change what goes
	// on the wire.
	Query url.Values
	// Header is merged into every request to this endpoint. New copies it, so
	// mutating the map afterwards does not change what goes on the wire.
	Header http.Header
	// Client performs the round trips. Nil selects httpcore's default.
	Client *http.Client
	// MaxBody caps the response size. Zero selects httpcore.DefaultMaxBody.
	MaxBody int64
	// Sensitive marks every Value from this endpoint as secret, driving
	// redaction downstream. It is per-endpoint because a generic HTTP endpoint
	// may serve either secrets or plain configuration and mamori cannot infer
	// which.
	Sensitive bool
	// AllowInsecure permits an http:// BaseURL. Fetching configuration in
	// cleartext exposes it to anything on the path, so it must be opted into.
	AllowInsecure bool
}

// endpoint is a validated Endpoint with its client and revalidator built.
//
// It does not keep the Name: the Provider's map is keyed by it, so the only
// code that needs the name already has it in hand.
type endpoint struct {
	query     url.Values
	header    http.Header
	sensitive bool
	reval     *httpcore.Revalidator
	// client is Endpoint.Client verbatim (nil when the caller left it unset,
	// in which case httpcore.New built its own internal default that this
	// package holds no reference to). It is kept only so Close can release its
	// idle connections; every actual request goes through reval, not this
	// field directly.
	client *http.Client
}

// Provider resolves https:// refs against registered endpoints. It is safe for
// concurrent use.
type Provider struct {
	endpoints map[string]*endpoint

	mu     sync.Mutex
	closed bool
}

// New validates endpoints and returns a Provider. Register it with
// mamori.WithProvider or mamori.Register.
//
// It fails when no endpoint is supplied, when a Name is empty, duplicated, or
// contains '/', '?' or '#', when a BaseURL is missing or unparsable, or when a
// BaseURL is http:// without AllowInsecure. Every one of those is a startup
// failure rather than a resolve failure, so a misconfiguration cannot reach
// production as an intermittent error.
//
// Query and Header are copied rather than retained, so a caller that keeps and
// mutates the map it passed cannot change what later requests send. The same
// reasoning makes httpcore.Revalidator clone bodies in both directions.
func New(endpoints ...Endpoint) (*Provider, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("https: at least one Endpoint is required: %w", mamori.ErrInvalid)
	}

	out := make(map[string]*endpoint, len(endpoints))
	for _, e := range endpoints {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("https: Endpoint.Name is required: %w", mamori.ErrInvalid)
		// '?' and '#' are rejected alongside '/' because mamori.ParseRef splits
		// a ref on them before the path is ever matched against a name. An
		// endpoint named "a?b" is usually unreachable, which fails loudly, but
		// not always: with both "a?b" and "a" registered, the ref
		// "https://a?b/cfg" is split at the '?' and routes silently to endpoint
		// "a" with an empty path. That returns mamori.ErrNotFound, and
		// ErrNotFound is the one kind that makes mamori apply the field's
		// default, so a misconfigured ref would quietly become a default value
		// instead of an error. Resolve's doc comment says exactly that must
		// never happen; the name is where it is cheapest to prevent.
		case strings.ContainsAny(e.Name, "/?#"):
			return nil, fmt.Errorf("https: Endpoint.Name %q must not contain '/', '?' or '#', it is the ref authority: %w", e.Name, mamori.ErrInvalid)
		}
		if _, dup := out[e.Name]; dup {
			return nil, fmt.Errorf("https: duplicate Endpoint.Name %q: %w", e.Name, mamori.ErrInvalid)
		}

		if e.BaseURL == "" {
			return nil, fmt.Errorf("https: endpoint %q BaseURL is required: %w", e.Name, mamori.ErrInvalid)
		}
		u, err := url.Parse(e.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("https: endpoint %q BaseURL %q is not a URL: %w: %w", e.Name, e.BaseURL, mamori.ErrInvalid, err)
		}
		// The scheme is checked against a closed set rather than only rejecting
		// http://. Anything else, an ftp:// typo or a ws:// paste, otherwise
		// passes here AND passes httpcore.New, which only requires a scheme and
		// a host, and then fails on every single resolve with net/http's
		// "unsupported protocol scheme". New exists precisely so a
		// misconfiguration cannot reach production as a resolve-time failure.
		switch u.Scheme {
		case "https":
		case "http":
			if !e.AllowInsecure {
				return nil, fmt.Errorf("https: endpoint %q BaseURL is http://, which sends configuration in cleartext; set AllowInsecure to accept that: %w", e.Name, mamori.ErrInvalid)
			}
		default:
			return nil, fmt.Errorf("https: endpoint %q BaseURL scheme %q is not http or https: %w", e.Name, u.Scheme, mamori.ErrInvalid)
		}

		client, err := httpcore.New(httpcore.Config{
			BaseURL:    e.BaseURL,
			HTTPClient: e.Client,
			Auth:       e.Auth,
			MaxBody:    e.MaxBody,
			UserAgent:  "mamori-https",
		})
		if err != nil {
			return nil, fmt.Errorf("https: endpoint %q: %w", e.Name, err)
		}

		out[e.Name] = &endpoint{
			query:     cloneQuery(e.Query),
			header:    e.Header.Clone(),
			sensitive: e.Sensitive,
			reval:     httpcore.NewRevalidator(client, 0),
			client:    e.Client,
		}
	}
	return &Provider{endpoints: out}, nil
}

// cloneQuery returns a deep copy of v, so an Endpoint.Query the caller still
// holds cannot change what later requests send.
//
// http.Header has Clone for this; url.Values does not, and maps.Clone would not
// do either, because a url.Values maps to a SLICE of values. A shallow copy
// leaves both maps pointing at the same backing arrays, so v["env"][0] = "prod"
// on the caller's copy still reaches the wire. A nil input stays nil, matching
// http.Header.Clone, so an endpoint with no query keeps an absent one rather
// than an empty map.
func cloneQuery(v url.Values) url.Values {
	if v == nil {
		return nil
	}
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}

// Scheme returns "https".
func (p *Provider) Scheme() string { return scheme }

// Close marks the provider closed and returns each endpoint's idle HTTP
// connections to its pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, without contacting any
// endpoint.
//
// An Endpoint.Client the caller supplied is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the
// caller's own use of that client is unaffected by closing this provider. An
// endpoint left with no Client uses an internal default httpcore.New built
// for it, which this package holds no reference to and therefore cannot
// (and need not) release here.
//
// CloseIdleConnections is also skipped for any endpoint whose Client has a
// nil Transport, since net/http resolves that to the process-global
// http.DefaultTransport, and releasing idle connections there would evict
// connections belonging to unrelated code elsewhere in the process rather
// than anything this endpoint used.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, ep := range p.endpoints {
		if ep.client != nil && ep.client.Transport != nil {
			ep.client.CloseIdleConnections()
		}
	}
	return nil
}
