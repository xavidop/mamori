package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig"
	"github.com/xavidop/mamori"
)

// SchemeAppConfig is the URL scheme handled by AppConfigProvider.
const SchemeAppConfig = "azure-appconfig"

// keyVaultRefContentType marks a setting whose value is a reference to a Key
// Vault secret rather than the value itself.
const keyVaultRefContentType = "application/vnd.microsoft.appconfig.keyvaultref+json"

// appConfigClient is the minimal subset of *azappconfig.Client this provider
// needs. The real SDK client satisfies it; tests inject an in-memory fake.
type appConfigClient interface {
	GetSetting(ctx context.Context, key string, opts *azappconfig.GetSettingOptions) (azappconfig.GetSettingResponse, error)
}

// AppConfigProvider resolves
// azure-appconfig://<store>/<key>[#json-key][?label=<label>] refs against
// Azure App Configuration. One provider serves every store named in a ref,
// lazily building and caching a client per store name, so construction never
// performs I/O and init-time registration is safe without Azure credentials
// present.
//
// AppConfigProvider is safe for concurrent use.
type AppConfigProvider struct {
	mu sync.Mutex

	clients map[string]appConfigClient
	fixed   appConfigClient

	cred         azcore.TokenCredential
	credErr      error
	credResolved bool

	newCredential func() (azcore.TokenCredential, error)
	newClient     func(endpoint string, cred azcore.TokenCredential) (appConfigClient, error)
}

// AppConfigOption configures an AppConfigProvider. It is distinct from Option,
// which configures the Key Vault provider in this same package.
type AppConfigOption func(*AppConfigProvider)

// WithAppConfigCredential injects an explicit token credential instead of the
// default Azure credential chain.
func WithAppConfigCredential(cred azcore.TokenCredential) AppConfigOption {
	return func(p *AppConfigProvider) {
		p.newCredential = func() (azcore.TokenCredential, error) { return cred, nil }
	}
}

// WithAppConfigClient injects a client used for every store, bypassing
// credential and client construction. It is primarily intended for tests.
func WithAppConfigClient(c appConfigClient) AppConfigOption {
	return func(p *AppConfigProvider) { p.fixed = c }
}

// NewAppConfig constructs an Azure App Configuration provider. With no options
// it uses the Azure default credential chain and builds a real azappconfig
// client per store lazily on first Resolve.
func NewAppConfig(opts ...AppConfigOption) *AppConfigProvider {
	p := &AppConfigProvider{
		clients: map[string]appConfigClient{},
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
	}
	p.newClient = func(endpoint string, cred azcore.TokenCredential) (appConfigClient, error) {
		return azappconfig.NewClient(endpoint, cred, nil)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// newAppConfigWithClient returns a provider backed by a caller-supplied
// client. It is the injection seam used by tests.
func newAppConfigWithClient(c appConfigClient) *AppConfigProvider {
	return NewAppConfig(WithAppConfigClient(c))
}

// Compile-time interface check.
var _ mamori.Provider = (*AppConfigProvider)(nil)

// Scheme returns "azure-appconfig".
func (p *AppConfigProvider) Scheme() string { return SchemeAppConfig }

// Resolve fetches a setting from Azure App Configuration. The ref path is
// "<store>/<key>", where the key may itself contain slashes. A #json-key
// fragment selects a field from a JSON payload, and ?label=<label> selects a
// labelled setting.
func (p *AppConfigProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	store, key, ok := strings.Cut(ref.Path, "/")
	if !ok || store == "" || key == "" {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: ref %q must be azure-appconfig://<store>/<key>[#json-key][?label=<label>]: %w",
			ref.Raw, mamori.ErrInvalid)
	}

	client, err := p.clientFor(store)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("azure-appconfig: building client for store %q: %w", store, err)
	}

	// An absent ?label= means Azure's null label, which is a distinct setting
	// from any labelled one, not a wildcard. Passing the empty string
	// explicitly requests that null label.
	label := ref.Opt("label")
	opts := &azappconfig.GetSettingOptions{Label: &label}

	resp, err := client.GetSetting(ctx, key, opts)
	if err != nil {
		if isNotFound(err) {
			return mamori.Value{}, fmt.Errorf(
				"azure-appconfig: setting %q in store %q not found: %w: %w",
				key, store, mamori.ErrNotFound, err)
		}
		return mamori.Value{}, fmt.Errorf("azure-appconfig: resolve %q: %w", ref.Path, classifyAzure(err))
	}
	if resp.Value == nil {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: setting %q in store %q has no value: %w", key, store, mamori.ErrNotFound)
	}

	// A Key Vault reference is a pointer to a secret, not the secret. Returning
	// its JSON would hand the caller {"uri":"..."} as their value, which passes
	// a non-empty-string validation and fails much later inside whatever
	// consumes it. Fail here instead, naming the ref they should have written.
	if resp.ContentType != nil && strings.HasPrefix(*resp.ContentType, keyVaultRefContentType) {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: setting %q in store %q is a Key Vault reference, which this provider does not resolve; %s: %w",
			key, store, keyVaultHint(*resp.Value), mamori.ErrInvalid)
	}

	data := []byte(*resp.Value)
	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	version := ""
	if resp.ETag != nil {
		version = string(*resp.ETag)
	}
	if version == "" {
		version = mamori.VersionHash(data)
	}

	meta := map[string]string{"store": store}
	if label != "" {
		meta["label"] = label
	}

	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a configuration service, not a secret store
		Metadata:  meta,
	}, nil
}

// keyVaultGenericHint is the advice given when the reference payload is not
// the shape the service documents, or is a shape this helper does not (yet)
// understand well enough to name a specific ref for.
const keyVaultGenericHint = "use an azure-kv:// ref for the referenced secret"

// keyVaultHint turns a Key Vault reference payload into the azure-kv:// ref the
// user should write instead. It degrades to keyVaultGenericHint whenever the
// payload is not the shape the service documents, since this only ever builds
// an error message and must never itself fail or panic.
func keyVaultHint(value string) string {
	var ref struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal([]byte(value), &ref); err != nil || ref.URI == "" {
		return keyVaultGenericHint
	}
	// https://<vault>.vault.azure.net/secrets/<name>[/<version>]
	rest, ok := strings.CutPrefix(ref.URI, "https://")
	if !ok {
		return keyVaultGenericHint
	}
	host, path, ok := strings.Cut(rest, "/")
	if !ok {
		return keyVaultGenericHint
	}
	vault, _, _ := strings.Cut(host, ".")
	if vault == "" {
		return keyVaultGenericHint
	}
	secretsPath, ok := strings.CutPrefix(path, "secrets/")
	if !ok {
		// Not a secret reference at all (e.g. a "keys/" or "certificates/"
		// reference) - do not guess at a name that was never a secret name.
		return keyVaultGenericHint
	}
	name, version, hasVersion := strings.Cut(secretsPath, "/")
	if name == "" {
		return keyVaultGenericHint
	}
	if hasVersion && version != "" {
		return fmt.Sprintf("use azure-kv://%s/%s?version=%s instead", vault, name, version)
	}
	return fmt.Sprintf("use azure-kv://%s/%s instead", vault, name)
}

// clientFor returns the client for the named store, creating and caching it on
// first use. When a fixed client was injected it is returned for every store.
func (p *AppConfigProvider) clientFor(store string) (appConfigClient, error) {
	if p.fixed != nil {
		return p.fixed, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[store]; ok {
		return c, nil
	}

	cred, err := p.credentialLocked()
	if err != nil {
		return nil, err
	}
	endpoint := "https://" + store + ".azconfig.io"
	c, err := p.newClient(endpoint, cred)
	if err != nil {
		return nil, err
	}
	p.clients[store] = c
	return c, nil
}

// credentialLocked resolves the token credential at most once. Callers must
// hold p.mu.
func (p *AppConfigProvider) credentialLocked() (azcore.TokenCredential, error) {
	if !p.credResolved {
		p.cred, p.credErr = p.newCredential()
		p.credResolved = true
	}
	return p.cred, p.credErr
}
