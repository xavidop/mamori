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
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the ref scheme this provider registers.
const scheme = "https"

// Endpoint is one operator-declared backend. Its Name is what a ref's authority
// selects.
type Endpoint struct {
	// Name is the ref authority, e.g. "billing" in https://billing/cfg. It must
	// be non-empty and must not contain '/'. Required.
	Name string
	// BaseURL is the root every ref path is joined onto. Required. An http://
	// BaseURL is rejected unless AllowInsecure is set.
	BaseURL string
	// Auth injects credentials. Nil sends requests unauthenticated.
	Auth httpcore.Authenticator
	// Query is merged into every request to this endpoint. It exists because a
	// ref cannot carry target query parameters; see the package documentation.
	Query url.Values
	// Header is merged into every request to this endpoint.
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
}

// Provider resolves https:// refs against registered endpoints. It is safe for
// concurrent use.
type Provider struct {
	endpoints map[string]*endpoint
}

// New validates endpoints and returns a Provider. Register it with
// mamori.WithProvider or mamori.Register.
//
// It fails when no endpoint is supplied, when a Name is empty, duplicated, or
// contains '/', when a BaseURL is missing or unparsable, or when a BaseURL is
// http:// without AllowInsecure. Every one of those is a startup failure rather
// than a resolve failure, so a misconfiguration cannot reach production as an
// intermittent error.
func New(endpoints ...Endpoint) (*Provider, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("https: at least one Endpoint is required: %w", mamori.ErrInvalid)
	}

	out := make(map[string]*endpoint, len(endpoints))
	for _, e := range endpoints {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("https: Endpoint.Name is required: %w", mamori.ErrInvalid)
		case strings.Contains(e.Name, "/"):
			return nil, fmt.Errorf("https: Endpoint.Name %q must not contain '/', it is the ref authority: %w", e.Name, mamori.ErrInvalid)
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
			query:     e.Query,
			header:    e.Header,
			sensitive: e.Sensitive,
			reval:     httpcore.NewRevalidator(client, 0),
		}
	}
	return &Provider{endpoints: out}, nil
}

// Scheme returns "https".
func (p *Provider) Scheme() string { return scheme }
