// Package onepassword implements a mamori Provider backed by 1Password Connect.
//
// It resolves refs of the form:
//
//	op://<vault>/<item>/<field>
//
// where <vault> is a vault name (or id), <item> is an item title (or id), and
// <field> is a field label (or id). For example:
//
//	DBPassword secret.String `source:"op://Production/postgres/password"`
//
// The provider talks to a 1Password Connect server over its REST API using
// github.com/xavidop/mamori/providers/httpcore and the Go standard library
// only, inheriting request building, status classification, body bounding and
// URL redaction from one shared place. The Connect host comes from
// OP_CONNECT_HOST and the access token from OP_CONNECT_TOKEN (sent as an
// "Authorization: Bearer" header). Values are always marked Sensitive.
// 1Password Connect has no native change notification, so this provider is not
// watchable; mamori polls it.
package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "op"

// Environment variables read for ambient configuration.
const (
	envHost  = "OP_CONNECT_HOST"
	envToken = "OP_CONNECT_TOKEN"
)

// errDetailLimit bounds how much of a failing response's body is quoted into
// an error message. Connect error bodies are diagnostic JSON, not secret
// values, but the bound is what stops a hostile or broken server turning that
// diagnostic into an unbounded string.
const errDetailLimit = 200

func init() { mamori.Register(New()) }

// Provider resolves op:// refs against a 1Password Connect server. It is safe
// for concurrent use.
type Provider struct {
	host  string
	token string
	hc    *http.Client

	mu     sync.Mutex
	closed bool
}

// Option configures a Provider.
type Option func(*Provider)

// WithHost sets the 1Password Connect base URL, e.g. "https://connect.example:8080".
// It overrides OP_CONNECT_HOST.
func WithHost(host string) Option { return func(p *Provider) { p.host = host } }

// WithToken sets the Connect access token. It overrides OP_CONNECT_TOKEN.
func WithToken(token string) Option { return func(p *Provider) { p.token = token } }

// WithHTTPClient injects a custom *http.Client (for timeouts, transports, or
// tests pointing at an httptest.Server).
func WithHTTPClient(hc *http.Client) Option {
	return func(p *Provider) {
		if hc != nil {
			p.hc = hc
		}
	}
}

// New constructs a Provider. Host and token are read lazily from the environment
// (OP_CONNECT_HOST / OP_CONNECT_TOKEN) at resolve time unless overridden with
// WithHost / WithToken, so init-time registration works before env is populated.
//
// Users needing explicit configuration register via:
//
//	mamori.WithProvider(onepassword.New(onepassword.WithHost("https://connect:8080"), onepassword.WithToken("...")))
func New(opts ...Option) *Provider {
	p := &Provider{
		hc: &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Scheme returns "op".
func (p *Provider) Scheme() string { return Scheme }

// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports
// errors.Is(err, mamori.ErrUnavailable) locally, through the same closed check
// clientFor already applies, without contacting Connect.
//
// A client supplied through WithHTTPClient is never invalidated: only its idle
// connections are released (Go's transport redials on demand), so the caller's
// own use of that client is unaffected by closing this provider.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.hc != nil {
		p.hc.CloseIdleConnections()
	}
	return nil
}

func (p *Provider) effectiveHost() string {
	if p.host != "" {
		return p.host
	}
	return os.Getenv(envHost)
}

func (p *Provider) effectiveToken() string {
	if p.token != "" {
		return p.token
	}
	return os.Getenv(envToken)
}

// clientFor builds the httpcore client for one resolve.
//
// It is built per call rather than cached because the host and token are read
// lazily from the environment on every resolve (see effectiveHost and
// effectiveToken): caching would pin whichever pair happened to be set on the
// first one. Construction performs no network call and reuses the provider's
// *http.Client, so the connection pool is shared across resolves regardless.
//
// The closed check runs first, so a closed provider refuses locally rather
// than building a client and reaching for the network.
func (p *Provider) clientFor(host, token string) (*httpcore.Client, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("%w: onepassword: provider is closed", mamori.ErrUnavailable)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     strings.TrimRight(host, "/"),
		HTTPClient:  p.hc,
		Auth:        httpcore.Bearer(token),
		ErrorDetail: errorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("onepassword: building request: %w", err)
	}
	return c, nil
}

// errorDetail is httpcore's Config.ErrorDetail hook: it quotes a bounded
// prefix of a failing response's body into the error message.
//
// Connect's error responses carry a free-text diagnostic message and no
// machine-readable error code, and they are answers to a lookup rather than to
// a request that contained a secret, so quoting them is safe. httpcore calls
// this only for a status it has already decided is a failure, never for the
// 200 whose body carries the item's field values.
func errorDetail(_ int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > errDetailLimit {
		msg = msg[:errDetailLimit]
	}
	return msg
}

// Resolve fetches the field named in ref from 1Password Connect. It returns an
// error satisfying errors.Is(err, mamori.ErrNotFound) when the vault, item, or
// field does not exist.
//
// A ref path containing a "." or ".." segment is rejected with
// mamori.ErrInvalid. That check is not here: httpcore.Client.Do enforces it for
// every provider built on it, in both the literal and the percent-encoded form,
// so no provider can forget it.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	host := p.effectiveHost()
	if host == "" {
		return mamori.Value{}, fmt.Errorf("onepassword: %s not set", envHost)
	}
	token := p.effectiveToken()
	if token == "" {
		return mamori.Value{}, fmt.Errorf("onepassword: %s not set", envToken)
	}

	vaultRef, itemRef, fieldRef, err := parseOpRef(ref)
	if err != nil {
		return mamori.Value{}, err
	}

	client, err := p.clientFor(host, token)
	if err != nil {
		return mamori.Value{}, err
	}

	vaultID, err := p.resolveVaultID(ctx, client, vaultRef)
	if err != nil {
		return mamori.Value{}, err
	}

	item, err := p.resolveItem(ctx, client, vaultID, itemRef)
	if err != nil {
		return mamori.Value{}, err
	}

	for _, f := range item.Fields {
		if f.Label == fieldRef || f.ID == fieldRef {
			version := strconv.Itoa(item.Version)
			if item.Version == 0 {
				version = mamori.VersionHash([]byte(f.Value))
			}
			return mamori.Value{
				Bytes:     []byte(f.Value),
				Version:   version,
				Sensitive: true,
				Metadata: map[string]string{
					"vault": vaultRef,
					"item":  itemRef,
					"field": fieldRef,
				},
			}, nil
		}
	}
	return mamori.Value{}, fmt.Errorf("onepassword: field %q not found in item %q: %w", fieldRef, itemRef, mamori.ErrNotFound)
}

// parseOpRef splits an op:// ref path into its vault, item, and field segments.
// ParseRef stores "op://vault/item/field" with Path == "vault/item/field" and an
// empty Key, so exactly three "/"-separated segments are expected.
func parseOpRef(ref mamori.Ref) (vault, item, field string, err error) {
	path := strings.Trim(ref.Path, "/")
	segs := strings.SplitN(path, "/", 3)
	if len(segs) != 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
		return "", "", "", fmt.Errorf("onepassword: ref %q must be of the form op://<vault>/<item>/<field>", ref.Raw)
	}
	return segs[0], segs[1], segs[2], nil
}

// --- Connect REST types ------------------------------------------------------

type vaultSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type itemSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

type itemField struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type item struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Version int         `json:"version"`
	Fields  []itemField `json:"fields"`
}

// resolveVaultID resolves a vault name to its id. If the name filter does not
// produce a match it treats the segment as a possible vault id and fetches it
// directly.
//
// A failed filter request is deliberately swallowed rather than returned: the
// filter is an optimization, and the direct fetch below is the authoritative
// answer for a segment that is an id rather than a name. That is what this
// provider has always done for a non-200 filter response; a transport failure
// now takes the same path as a status failure, since httpcore reports both as
// an error, and the direct fetch fails identically a moment later.
func (p *Provider) resolveVaultID(ctx context.Context, c *httpcore.Client, vaultRef string) (string, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`name eq "%s"`, vaultRef))
	if body, err := get(ctx, c, "/v1/vaults", q); err == nil {
		var vaults []vaultSummary
		if err := json.Unmarshal(body, &vaults); err != nil {
			return "", fmt.Errorf("onepassword: decoding vaults: %w", err)
		}
		if len(vaults) > 0 {
			return vaults[0].ID, nil
		}
	}

	// Fall back to treating vaultRef as a vault id.
	body, err := get(ctx, c, "/v1/vaults/"+url.PathEscape(vaultRef), nil)
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			return "", fmt.Errorf("onepassword: vault %q not found: %w", vaultRef, mamori.ErrNotFound)
		}
		return "", fmt.Errorf("onepassword: vault lookup: %w", err)
	}
	var v vaultSummary
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("onepassword: decoding vault: %w", err)
	}
	if v.ID != "" {
		return v.ID, nil
	}
	return vaultRef, nil
}

// resolveItem finds an item by title within a vault and fetches it in full
// (including its fields). A failed title-filter request falls back to treating
// itemRef as an item id, for the reason resolveVaultID's doc comment gives.
func (p *Provider) resolveItem(ctx context.Context, c *httpcore.Client, vaultID, itemRef string) (item, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`title eq "%s"`, itemRef))

	var itemID string
	if body, err := get(ctx, c, "/v1/vaults/"+url.PathEscape(vaultID)+"/items", q); err == nil {
		var items []itemSummary
		if err := json.Unmarshal(body, &items); err != nil {
			return item{}, fmt.Errorf("onepassword: decoding items: %w", err)
		}
		if len(items) > 0 {
			itemID = items[0].ID
		}
	}
	if itemID == "" {
		// Fall back to treating itemRef as an item id.
		itemID = itemRef
	}

	body, err := get(ctx, c, "/v1/vaults/"+url.PathEscape(vaultID)+"/items/"+url.PathEscape(itemID), nil)
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			return item{}, fmt.Errorf("onepassword: item %q not found: %w", itemRef, mamori.ErrNotFound)
		}
		return item{}, fmt.Errorf("onepassword: item lookup: %w", err)
	}
	var it item
	if err := json.Unmarshal(body, &it); err != nil {
		return item{}, fmt.Errorf("onepassword: decoding item: %w", err)
	}
	return it, nil
}

// get performs one GET against the Connect API and returns the response body.
// A non-2xx status comes back as an error already carrying the mamori sentinel
// httpcore classified it with, so callers test it with errors.Is rather than
// switching on a status code.
func get(ctx context.Context, c *httpcore.Client, path string, query url.Values) ([]byte, error) {
	resp, err := c.Do(ctx, httpcore.Request{
		Path:   path,
		Query:  query,
		Header: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		// One %w: the cause already carries the sentinel that classifies it.
		return nil, err
	}
	return resp.Body, nil
}

// Ensure Provider satisfies the core interface. Note: no Watch method is
// implemented because 1Password Connect has no native change notification;
// mamori polls the provider instead.
var _ mamori.Provider = (*Provider)(nil)
