// Package httpcore is the shared HTTP resolve core for mamori providers whose
// backend is a REST API.
//
// Sixteen of mamori's providers speak HTTP, and before this package each one
// hand-rolled request building, credential injection, status classification,
// and response body hygiene. That duplication is what issue #107 surfaced as
// inconsistent body draining. This package exists so a provider author writes
// the part that is actually specific to their backend and inherits the rest.
//
// # What it does not do
//
// It does not retry. mamori's reconciler already backs off and retries a failed
// resolve (see backoff.go in the core module), and a second retry layer inside
// the provider would multiply against it, turning a configured five attempts
// into twenty-five.
//
// It does not parse vendor error envelopes. [ClassifyStatus] takes the detail
// string from its caller, and [Config.ErrorDetail] is where a provider supplies
// one, because a response body can contain the resolved value itself and only
// the provider knows which field of its backend's error shape is safe to
// surface. Leaving that hook nil means no body ever reaches an error.
//
// # Units
//
//   - [Client] performs one round trip with a bounded, always-drained body.
//   - [Authenticator] injects credentials.
//   - [ClassifyStatus] maps an HTTP status onto a mamori error sentinel.
//   - [Revalidator] turns a repeated poll into a conditional GET.
package httpcore

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/xavidop/mamori"
)

// DefaultMaxBody is the response size ceiling applied when Config.MaxBody is not
// set. A configuration value that does not fit in a megabyte is a mistake, and
// the ceiling is what stops a broken or hostile backend from exhausting memory.
const DefaultMaxBody int64 = 1 << 20

// DefaultTimeout is the per-request timeout applied when Config.HTTPClient is
// not supplied.
const DefaultTimeout = 30 * time.Second

// Config constructs a Client.
type Config struct {
	// BaseURL is the root every Request.Path is joined onto. Required.
	BaseURL string
	// HTTPClient performs the round trips. When nil, a client with
	// DefaultTimeout is used. Supply one to control transport, proxy, or TLS.
	HTTPClient *http.Client
	// Auth injects credentials. When nil, requests are sent unauthenticated.
	Auth Authenticator
	// MaxBody caps how many response bytes are read. Zero or negative selects
	// DefaultMaxBody.
	MaxBody int64
	// UserAgent sets the User-Agent header. Empty leaves Go's default.
	UserAgent string
	// ErrorDetail turns a failing response's body into the detail string
	// [ClassifyStatus] appends to its error. It is called only for a status
	// ClassifyStatus rejects, and only ever with at most MaxBody bytes of that
	// body, so a huge error body cannot defeat the ceiling.
	//
	// Nil means no detail, and nil is the safe default because a response body
	// can be the resolved value itself: a config value, a secret, a token.
	// Surfacing it unconditionally would put that value in an error message,
	// a log line, and a mamori status page. Only the provider knows which
	// field of its backend's error envelope, if any, cannot contain the value,
	// which is exactly why this is a hook rather than something httpcore
	// decides. Returning "" suppresses the detail for that one response.
	//
	// It exists because several providers already embed a bounded error body
	// in their message, a house convention shared by providers/doppler,
	// providers/cloudflare-kv, providers/vercel-gc, and providers/scaleway-sm,
	// and a migration onto httpcore must be able to preserve it.
	ErrorDetail func(status int, body []byte) string
}

// Client performs bounded, classified HTTP round trips against one backend. It
// is safe for concurrent use.
//
// Client does not retry. See the package documentation for why.
type Client struct {
	base        *url.URL
	http        *http.Client
	auth        Authenticator
	maxBody     int64
	userAgent   string
	errorDetail func(status int, body []byte) string
}

// New validates cfg and returns a Client. It fails when BaseURL is empty or
// cannot be parsed, so a misconfiguration surfaces at construction rather than
// at the first resolve.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("httpcore: BaseURL is required: %w", mamori.ErrInvalid)
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("httpcore: BaseURL %q is not a URL: %w: %w", cfg.BaseURL, mamori.ErrInvalid, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("httpcore: BaseURL %q needs a scheme and host: %w", cfg.BaseURL, mamori.ErrInvalid)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	maxBody := cfg.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}

	return &Client{
		base:        base,
		http:        hc,
		auth:        cfg.Auth,
		maxBody:     maxBody,
		userAgent:   cfg.UserAgent,
		errorDetail: cfg.ErrorDetail,
	}, nil
}
