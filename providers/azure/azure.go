// Package azure provides mamori Providers for two Azure services: Key Vault
// secrets and App Configuration settings.
//
// The Key Vault provider (Provider, registered under Scheme = "azure-kv")
// resolves refs of the form:
//
//	azure-kv://<vault-name>/<secret-name>[#json-key]?version=<v>
//
// The vault name is turned into the vault URL https://<vault-name>.vault.azure.net
// and the secret is fetched with the azsecrets SDK. When #json-key is present the
// secret payload is treated as a JSON object and the named field is selected with
// mamori.SelectKey, identically to every other mamori provider. The optional
// ?version=<v> query pins a specific secret version; when omitted the latest
// version is returned.
//
// The App Configuration provider (AppConfigProvider, registered under
// SchemeAppConfig = "azure-appconfig") resolves refs of the form:
//
//	azure-appconfig://<store>/<key>[#json-key][?label=<label>]
//
// The store name is turned into the endpoint https://<store>.azconfig.io and
// the setting is fetched with the azappconfig SDK. An absent ?label= requests
// Azure's null label, not any label, since the two are distinct settings in
// the service. A setting stored as a Key Vault reference (content type
// application/vnd.microsoft.appconfig.keyvaultref+json) is never resolved;
// Resolve fails with mamori.ErrInvalid naming the equivalent azure-kv:// ref.
//
// Key Vault values are always marked Sensitive; App Configuration values never
// are, since App Configuration is a configuration service, not a secret store.
// Neither provider implements Watch, since neither Azure service has a native
// change notification usable here; mamori polls both instead.
package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "azure-kv"

func init() {
	mamori.Register(New())
	mamori.Register(NewAppConfig())
}

// kvClient is the minimal subset of *azsecrets.Client this provider needs. The
// real SDK client satisfies it, and tests inject an in-memory fake.
type kvClient interface {
	GetSecret(ctx context.Context, name, version string, opts *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}

// Provider resolves azure-kv:// refs against Azure Key Vault. A single Provider
// serves every vault named in a ref; it lazily builds (and caches) one client
// per vault name using an ambient credential, so construction never performs I/O
// and init-time registration is safe even without Azure credentials present.
//
// Provider is safe for concurrent use.
type Provider struct {
	mu sync.Mutex

	// clients caches one client per vault name (production path).
	clients map[string]kvClient

	// fixed, when set via WithClient, is used for every vault (test path).
	fixed kvClient

	// credential resolution (lazy, resolved at most once).
	cred         azcore.TokenCredential
	credErr      error
	credResolved bool

	// newCredential builds the token credential on first use. Overridable via
	// WithCredential; defaults to the Azure default credential chain.
	newCredential func() (azcore.TokenCredential, error)

	// newClient builds a client for a vault URL. Overridable in tests; defaults
	// to azsecrets.NewClient.
	newClient func(vaultURL string, cred azcore.TokenCredential) (kvClient, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithCredential injects an explicit azcore.TokenCredential (e.g. a specific
// azidentity credential) instead of the default credential chain.
func WithCredential(cred azcore.TokenCredential) Option {
	return func(p *Provider) {
		p.newCredential = func() (azcore.TokenCredential, error) { return cred, nil }
	}
}

// WithClient injects a client used for every vault, bypassing credential and
// client construction. It is primarily intended for tests (inject an in-memory
// fake), but can also be used to supply a pre-configured *azsecrets.Client.
func WithClient(c kvClient) Option {
	return func(p *Provider) { p.fixed = c }
}

// New constructs a Provider. With no options it uses the Azure default
// credential chain (azidentity.NewDefaultAzureCredential) and builds a real
// azsecrets client per vault lazily on first Resolve.
//
// Users who need explicit configuration register it via
// mamori.WithProvider(azure.New(azure.WithCredential(cred))).
func New(opts ...Option) *Provider {
	p := &Provider{
		clients: map[string]kvClient{},
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
	}
	p.newClient = func(vaultURL string, cred azcore.TokenCredential) (kvClient, error) {
		return azsecrets.NewClient(vaultURL, cred, nil)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Scheme reports the URL scheme this provider handles.
func (p *Provider) Scheme() string { return Scheme }

// Resolve fetches a secret from Azure Key Vault. The ref path must be
// "<vault-name>/<secret-name>". A #key fragment selects a field from a JSON
// payload, and ?version=<v> pins a specific version. A missing secret is
// reported as an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	vault, secret, ok := strings.Cut(ref.Path, "/")
	if !ok || vault == "" || secret == "" {
		return mamori.Value{}, fmt.Errorf("azure-kv: ref %q must be azure-kv://<vault-name>/<secret-name>[#key][?version=v]: %w", ref.Raw, mamori.ErrInvalid)
	}

	client, err := p.clientFor(vault)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("azure-kv: building client for vault %q: %w", vault, err)
	}

	version := ref.Opt("version") // empty = latest
	resp, err := client.GetSecret(ctx, secret, version, nil)
	if err != nil {
		if isNotFound(err) {
			return mamori.Value{}, fmt.Errorf("azure-kv: secret %q in vault %q not found: %w: %w", secret, vault, mamori.ErrNotFound, err)
		}
		return mamori.Value{}, fmt.Errorf("azure-kv: resolve %q: %w", ref.Path, classifyAzure(err))
	}
	if resp.Value == nil {
		return mamori.Value{}, fmt.Errorf("azure-kv: secret %q in vault %q has no value: %w", secret, vault, mamori.ErrNotFound)
	}

	data := []byte(*resp.Value)
	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	// Prefer the native secret version; fall back to a content hash.
	ver := ""
	if resp.ID != nil {
		ver = resp.ID.Version()
	}
	if ver == "" {
		ver = mamori.VersionHash(data)
	}

	meta := map[string]string{"vault": vault}
	if resp.ContentType != nil && *resp.ContentType != "" {
		meta["contentType"] = *resp.ContentType
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: true,
		Metadata:  meta,
	}, nil
}

// clientFor returns the client for the named vault, creating and caching it on
// first use. When a fixed client was injected it is returned for every vault.
func (p *Provider) clientFor(vault string) (kvClient, error) {
	if p.fixed != nil {
		return p.fixed, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[vault]; ok {
		return c, nil
	}

	cred, err := p.credentialLocked()
	if err != nil {
		return nil, err
	}
	vaultURL := "https://" + vault + ".vault.azure.net"
	c, err := p.newClient(vaultURL, cred)
	if err != nil {
		return nil, err
	}
	p.clients[vault] = c
	return c, nil
}

// credentialLocked resolves the token credential at most once. Callers must hold
// p.mu.
func (p *Provider) credentialLocked() (azcore.TokenCredential, error) {
	if !p.credResolved {
		p.cred, p.credErr = p.newCredential()
		p.credResolved = true
	}
	return p.cred, p.credErr
}

// isNotFound reports whether err is an Azure 404 (secret does not exist).
func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

// classifyAzure maps a Key Vault HTTP status onto a mamori classification
// sentinel. Key Vault returns 403 both for a token that lacks an access policy
// and for a firewall rejection; both are correctly permission_denied.
//
// A transport failure is not an *azcore.ResponseError at all and is left
// unclassified, since it could equally be a provider bug as a backend outage.
func classifyAzure(err error) error {
	if err == nil {
		return nil
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return err
	}
	var sentinel error
	switch code := respErr.StatusCode; {
	case code == http.StatusNotFound:
		sentinel = mamori.ErrNotFound
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
