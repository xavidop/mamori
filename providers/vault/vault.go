// Package vault implements the mamori Provider for HashiCorp Vault's KV v2
// secrets engine.
//
// Refs use the scheme:
//
//	vault://<mount>/<path>[#key][?renew=true]
//
// where <mount> is the KV v2 mount point (e.g. "secret") and <path> is the
// logical secret path within that mount. mamori reads the physical KV v2
// location <mount>/data/<path> for you, so you never write "/data/" in the ref
// (a leading "data/" in the path is tolerated and stripped for convenience).
//
//	Token    string `source:"vault://secret/myapp/config#token"`
//	FullJSON []byte `source:"vault://secret/myapp/config"`
//	Dynamic  string `source:"vault://database/creds/readonly?renew=true"`
//
// When #key is present the named field of the secret's data map is returned
// (via mamori.SelectKey, so selection behaves identically to every other
// provider). With no #key the whole data map is returned as its JSON encoding.
//
// Values are always marked Sensitive. When the underlying Vault response
// carries a lease (LeaseDuration > 0, as with dynamic secrets), Value.NotAfter
// is set to now+LeaseDuration so mamori refreshes before the lease expires.
// With ?renew=true and a renewable LeaseID, the lease is renewed on read.
//
// Vault KV has no native change notification, so this provider intentionally
// does NOT implement WatchableProvider: mamori polls, and NotAfter drives
// pre-expiry refresh (lease-aware polling). See the README.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "vault"

func init() { mamori.Register(New()) }

// kvReader is the minimal surface of the Vault client this provider uses. The
// real *vaultapi.Client is adapted to it by apiClient; tests inject an
// in-memory fake so conformance runs without a live Vault.
type kvReader interface {
	// Get reads the KV v2 secret at mount/path. It must return an error
	// satisfying errors.Is(err, vaultapi.ErrSecretNotFound) when the secret does
	// not exist.
	Get(ctx context.Context, mount, path string) (*vaultapi.KVSecret, error)
	// Renew renews the lease identified by leaseID, requesting increment seconds.
	Renew(ctx context.Context, leaseID string, increment int) (*vaultapi.Secret, error)
}

// apiClient adapts a *vaultapi.Client to kvReader.
type apiClient struct{ c *vaultapi.Client }

func (a apiClient) Get(ctx context.Context, mount, path string) (*vaultapi.KVSecret, error) {
	return a.c.KVv2(mount).Get(ctx, path)
}

func (a apiClient) Renew(ctx context.Context, leaseID string, increment int) (*vaultapi.Secret, error) {
	return a.c.Sys().RenewWithContext(ctx, leaseID, increment)
}

// Provider resolves vault:// refs against a Vault KV v2 engine.
type Provider struct {
	mu        sync.Mutex
	client    kvReader // resolved lazily on first use unless injected
	address   string
	token     string
	namespace string
}

// Option configures a Provider.
type Option func(*Provider)

// WithAddress sets the Vault server address (overriding VAULT_ADDR).
func WithAddress(addr string) Option { return func(p *Provider) { p.address = addr } }

// WithToken sets the Vault token (overriding VAULT_TOKEN).
func WithToken(token string) Option { return func(p *Provider) { p.token = token } }

// WithNamespace sets the Vault Enterprise namespace.
func WithNamespace(ns string) Option { return func(p *Provider) { p.namespace = ns } }

// WithClient injects a preconfigured *vaultapi.Client, bypassing lazy
// construction from the environment. Use this to supply custom auth, TLS, or a
// namespace-scoped client:
//
//	mamori.WithProvider(vault.New(vault.WithClient(myClient)))
func WithClient(c *vaultapi.Client) Option {
	return func(p *Provider) { p.client = apiClient{c: c} }
}

// New constructs a Provider. Without WithClient, the underlying *vaultapi.Client
// is built lazily on first Resolve from VAULT_ADDR / VAULT_TOKEN (plus any
// WithAddress / WithToken / WithNamespace overrides).
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// newWithReader builds a Provider around an arbitrary kvReader. Used by tests to
// inject an in-memory fake.
func newWithReader(r kvReader) *Provider { return &Provider{client: r} }

// Scheme returns "vault".
func (p *Provider) Scheme() string { return Scheme }

// reader returns the client, constructing the real one lazily on first use.
func (p *Provider) reader() (kvReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg := vaultapi.DefaultConfig() // reads VAULT_ADDR and standard VAULT_* env
	if cfg.Error != nil {
		return nil, cfg.Error
	}
	if p.address != "" {
		cfg.Address = p.address
	}
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	token := p.token
	if token == "" {
		token = os.Getenv("VAULT_TOKEN")
	}
	if token != "" {
		c.SetToken(token)
	}
	if p.namespace != "" {
		c.SetNamespace(p.namespace)
	}
	p.client = apiClient{c: c}
	return p.client, nil
}

// splitMountPath splits a ref path "<mount>/<path>" into its parts. A leading
// "data/" segment in the path portion (the KV v2 physical form) is tolerated and
// stripped, so both vault://secret/app and vault://secret/data/app resolve the
// same logical secret.
func splitMountPath(raw string) (mount, path string, err error) {
	trimmed := strings.Trim(raw, "/")
	mount, path, ok := strings.Cut(trimmed, "/")
	if !ok || mount == "" || path == "" {
		return "", "", fmt.Errorf("vault: ref path %q must be of the form <mount>/<path>", raw)
	}
	path = strings.TrimPrefix(path, "data/")
	if path == "" {
		return "", "", fmt.Errorf("vault: ref path %q has empty secret path after mount", raw)
	}
	return mount, path, nil
}

// Resolve fetches the secret named by ref from Vault KV v2.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	mount, path, err := splitMountPath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	client, err := p.reader()
	if err != nil {
		return mamori.Value{}, err
	}

	secret, err := client.Get(ctx, mount, path)
	if err != nil {
		if isNotFound(err) {
			return mamori.Value{}, fmt.Errorf("vault: secret %q: %w", ref.Path, mamori.ErrNotFound)
		}
		return mamori.Value{}, err
	}
	if secret == nil || secret.Data == nil {
		return mamori.Value{}, fmt.Errorf("vault: secret %q: %w", ref.Path, mamori.ErrNotFound)
	}

	// The data map is the secret. Encode it to JSON as the canonical payload.
	payload, err := json.Marshal(secret.Data)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("vault: encoding secret data: %w", err)
	}

	// #key selection is delegated to mamori.SelectKey for cross-provider parity.
	bytes := payload
	if ref.Key != "" {
		bytes, err = mamori.SelectKey(payload, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	v := mamori.Value{
		Bytes:     bytes,
		Version:   versionString(secret),
		Sensitive: true,
		Metadata:  map[string]string{"vault.mount": mount},
	}

	// Lease awareness: dynamic secrets carry a lease on the raw response.
	if raw := secret.Raw; raw != nil && raw.LeaseDuration > 0 {
		lease := raw.LeaseDuration
		if ref.Opt("renew") == "true" && raw.LeaseID != "" && raw.Renewable {
			if renewed, rerr := client.Renew(ctx, raw.LeaseID, raw.LeaseDuration); rerr == nil && renewed != nil {
				if renewed.LeaseDuration > 0 {
					lease = renewed.LeaseDuration
				}
				v.Metadata["vault.renewed"] = "true"
			}
			// A renew failure is non-fatal: the read itself succeeded, so we keep
			// the value and the original lease-derived NotAfter.
		}
		v.NotAfter = time.Now().Add(time.Duration(lease) * time.Second)
	}

	return v, nil
}

// versionString renders the KV v2 metadata version as a string. It falls back to
// a content hash when no metadata version is available, so change detection
// still works.
func versionString(s *vaultapi.KVSecret) string {
	if s.VersionMetadata != nil && s.VersionMetadata.Version > 0 {
		return strconv.Itoa(s.VersionMetadata.Version)
	}
	// No KV v2 version metadata (e.g. a plain logical/dynamic read): hash the
	// data so Value.Version still changes when the value changes.
	b, err := json.Marshal(s.Data)
	if err != nil {
		return ""
	}
	return mamori.VersionHash(b)
}

// isNotFound reports whether err represents a missing secret.
func isNotFound(err error) bool {
	if errors.Is(err, vaultapi.ErrSecretNotFound) {
		return true
	}
	var respErr *vaultapi.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == 404 {
		return true
	}
	return false
}
