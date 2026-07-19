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
// The provider talks to a 1Password Connect server over its REST API using only
// the Go standard library. The Connect host comes from OP_CONNECT_HOST and the
// access token from OP_CONNECT_TOKEN (sent as an "Authorization: Bearer" header).
// Values are always marked Sensitive. 1Password Connect has no native change
// notification, so this provider is not watchable; mamori polls it.
package onepassword

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "op"

// Environment variables read for ambient configuration.
const (
	envHost  = "OP_CONNECT_HOST"
	envToken = "OP_CONNECT_TOKEN"
)

func init() { mamori.Register(New()) }

// Provider resolves op:// refs against a 1Password Connect server. It is safe
// for concurrent use.
type Provider struct {
	host  string
	token string
	hc    *http.Client
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

// Resolve fetches the field named in ref from 1Password Connect. It returns an
// error satisfying errors.Is(err, mamori.ErrNotFound) when the vault, item, or
// field does not exist.
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

	vaultID, err := p.resolveVaultID(ctx, host, token, vaultRef)
	if err != nil {
		return mamori.Value{}, err
	}

	item, err := p.resolveItem(ctx, host, token, vaultID, itemRef)
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

// resolveVaultID resolves a vault name to its id. If the name filter returns no
// match it treats the segment as a possible vault id and fetches it directly.
func (p *Provider) resolveVaultID(ctx context.Context, host, token, vaultRef string) (string, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`name eq "%s"`, vaultRef))
	status, body, err := p.get(ctx, host, token, "/v1/vaults", q)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		var vaults []vaultSummary
		if err := json.Unmarshal(body, &vaults); err != nil {
			return "", fmt.Errorf("onepassword: decoding vaults: %w", err)
		}
		if len(vaults) > 0 {
			return vaults[0].ID, nil
		}
	}

	// Fall back to treating vaultRef as a vault id.
	status, body, err = p.get(ctx, host, token, "/v1/vaults/"+url.PathEscape(vaultRef), nil)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		var v vaultSummary
		if err := json.Unmarshal(body, &v); err != nil {
			return "", fmt.Errorf("onepassword: decoding vault: %w", err)
		}
		if v.ID != "" {
			return v.ID, nil
		}
		return vaultRef, nil
	case http.StatusNotFound:
		return "", fmt.Errorf("onepassword: vault %q not found: %w", vaultRef, mamori.ErrNotFound)
	default:
		return "", statusError("vault lookup", status, body)
	}
}

// resolveItem finds an item by title within a vault and fetches it in full
// (including its fields).
func (p *Provider) resolveItem(ctx context.Context, host, token, vaultID, itemRef string) (item, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`title eq "%s"`, itemRef))
	status, body, err := p.get(ctx, host, token, "/v1/vaults/"+url.PathEscape(vaultID)+"/items", q)
	if err != nil {
		return item{}, err
	}

	var itemID string
	if status == http.StatusOK {
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

	status, body, err = p.get(ctx, host, token, "/v1/vaults/"+url.PathEscape(vaultID)+"/items/"+url.PathEscape(itemID), nil)
	if err != nil {
		return item{}, err
	}
	switch status {
	case http.StatusOK:
		var it item
		if err := json.Unmarshal(body, &it); err != nil {
			return item{}, fmt.Errorf("onepassword: decoding item: %w", err)
		}
		return it, nil
	case http.StatusNotFound:
		return item{}, fmt.Errorf("onepassword: item %q not found: %w", itemRef, mamori.ErrNotFound)
	default:
		return item{}, statusError("item lookup", status, body)
	}
}

// get performs a GET against the Connect API and returns the status code and body.
func (p *Provider) get(ctx context.Context, host, token, path string, query url.Values) (int, []byte, error) {
	u := strings.TrimRight(host, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("onepassword: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("onepassword: reading response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// statusError builds an error for an unexpected HTTP status without leaking any
// secret payload (Connect error bodies are diagnostic JSON, not secret values).
func statusError(op string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("onepassword: %s: unexpected status %d: %s", op, status, msg)
}

// Ensure Provider satisfies the core interface. Note: no Watch method is
// implemented because 1Password Connect has no native change notification;
// mamori polls the provider instead.
var _ mamori.Provider = (*Provider)(nil)
