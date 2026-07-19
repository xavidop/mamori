// Package azblob provides a mamori Provider for Azure Blob Storage.
//
// It resolves refs of the form:
//
//	azblob://<container>/<blob>[#json-key]
//
// The blob is downloaded with the azblob SDK (DownloadStream) and its bytes
// become the resolved Value. The <blob> segment may itself contain slashes
// (blob names are flat but conventionally use "/" as a virtual-directory
// separator), so only the first slash of the ref path separates the container
// from the blob name.
//
// The storage account is provider-level configuration, not part of the ref: set
// it with WithAccountURL / WithServiceURL, or via the AZURE_STORAGE_ACCOUNT
// environment variable (an account name, expanded to
// https://<account>.blob.core.windows.net). This lets the same ref resolve
// against different accounts across environments.
//
// When #json-key is present the blob payload is treated as a JSON object and the
// named field is selected with mamori.SelectKey, identically to every other
// mamori provider.
//
// Values are not marked Sensitive by default (blobs commonly hold ordinary
// config); pass WithSensitive(true) for buckets that hold secrets. The Value
// Version is the blob ETag (or its VersionID when versioning is enabled),
// falling back to a content hash, so mamori detects changes cheaply on each
// poll. Azure Blob Storage has no cheap native push for a single blob, so this
// provider does not implement Watch; mamori polls it (an unchanged ETag makes
// each poll cheap). Event Grid blob events are a possible future push mode.
package azblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azblobsdk "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "azblob"

// AccountEnv is the environment variable consulted for the default storage
// account name when no account URL is supplied via options. Its value is an
// account name (e.g. "mystorageacct"), expanded to the blob service endpoint
// https://<account>.blob.core.windows.net.
const AccountEnv = "AZURE_STORAGE_ACCOUNT"

func init() { mamori.Register(New()) }

// blobDownloader downloads the contents of a single blob. It returns the raw
// bytes and a change-detection tag (the blob ETag, or its VersionID). It MUST
// return an error satisfying errors.Is(err, mamori.ErrNotFound) when the blob or
// its container does not exist. The production adapter wraps the azblob SDK;
// tests inject an in-memory fake. All parameter and result types are built-in,
// so callers outside this package can implement it too.
type blobDownloader interface {
	Download(ctx context.Context, container, blob string) (data []byte, etag string, err error)
}

// Provider resolves azblob:// refs against a single Azure Blob Storage account.
// The account endpoint is fixed for the Provider; the container and blob come
// from each ref. The real SDK client is built lazily on first Resolve using an
// ambient credential, so construction performs no I/O and init-time registration
// is safe even without Azure credentials present.
//
// Provider is safe for concurrent use.
type Provider struct {
	mu sync.Mutex

	// accountURL is the blob service endpoint, e.g.
	// https://<account>.blob.core.windows.net. It may be empty when neither an
	// option nor AZURE_STORAGE_ACCOUNT supplied one, in which case Resolve fails
	// with a clear configuration error.
	accountURL string

	// sensitive marks resolved values as secret (default false).
	sensitive bool

	// fixed, when set via WithClient, is the downloader used for every blob
	// (test path / injected client); it bypasses credential and client building.
	fixed blobDownloader

	// lazily-built real downloader (production path), resolved at most once.
	client    blobDownloader
	clientErr error
	built     bool

	// newCredential builds the token credential on first use. Overridable via
	// WithCredential; defaults to the Azure default credential chain.
	newCredential func() (azcore.TokenCredential, error)

	// newClient builds a downloader for an account URL. Overridable in tests;
	// defaults to a wrapper around azblob.NewClient.
	newClient func(accountURL string, cred azcore.TokenCredential) (blobDownloader, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithAccountURL sets the blob service endpoint. It accepts either a full URL
// (https://<account>.blob.core.windows.net) or a bare account name, which is
// expanded to that URL.
func WithAccountURL(url string) Option {
	return func(p *Provider) { p.accountURL = serviceURL(url) }
}

// WithServiceURL is an alias for WithAccountURL, matching the Azure SDK's
// "service URL" terminology.
func WithServiceURL(url string) Option { return WithAccountURL(url) }

// WithSensitive controls whether resolved values are marked Sensitive (driving
// redaction downstream). It defaults to false; pass true for accounts that hold
// secret material.
func WithSensitive(sensitive bool) Option {
	return func(p *Provider) { p.sensitive = sensitive }
}

// WithCredential injects an explicit azcore.TokenCredential (e.g. a specific
// azidentity credential) instead of the default credential chain.
func WithCredential(cred azcore.TokenCredential) Option {
	return func(p *Provider) {
		p.newCredential = func() (azcore.TokenCredential, error) { return cred, nil }
	}
}

// WithClient injects a downloader used for every blob, bypassing credential and
// client construction. It is primarily intended for tests (inject an in-memory
// fake), but can also supply a custom adapter.
func WithClient(c blobDownloader) Option {
	return func(p *Provider) { p.fixed = c }
}

// New constructs a Provider. With no options it reads the default storage
// account from AZURE_STORAGE_ACCOUNT, uses the Azure default credential chain
// (azidentity.NewDefaultAzureCredential), and builds a real azblob client lazily
// on first Resolve.
//
// Users who need explicit configuration register it via
// mamori.WithProvider(azblob.New(azblob.WithAccountURL(...))).
func New(opts ...Option) *Provider {
	p := &Provider{
		accountURL: serviceURL(os.Getenv(AccountEnv)),
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
	}
	p.newClient = func(accountURL string, cred azcore.TokenCredential) (blobDownloader, error) {
		c, err := azblobsdk.NewClient(accountURL, cred, nil)
		if err != nil {
			return nil, err
		}
		return &sdkDownloader{client: c}, nil
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Scheme reports the URL scheme this provider handles.
func (p *Provider) Scheme() string { return Scheme }

// Resolve downloads a blob from Azure Blob Storage. The ref path must be
// "<container>/<blob>"; the blob name may contain further slashes. A #key
// fragment selects a field from a JSON payload. A missing blob or container is
// reported as an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	container, blob, ok := strings.Cut(ref.Path, "/")
	if !ok || container == "" || blob == "" {
		return mamori.Value{}, fmt.Errorf("azblob: ref %q must be azblob://<container>/<blob>[#key]", ref.Raw)
	}

	d, err := p.downloader()
	if err != nil {
		return mamori.Value{}, fmt.Errorf("azblob: building client: %w", err)
	}

	data, etag, err := d.Download(ctx, container, blob)
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			return mamori.Value{}, fmt.Errorf("azblob: blob %q in container %q not found: %w", blob, container, mamori.ErrNotFound)
		}
		return mamori.Value{}, err
	}

	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	// Prefer the blob's native ETag/VersionID; fall back to a content hash.
	ver := etag
	if ver == "" {
		ver = mamori.VersionHash(data)
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: p.sensitive,
		Metadata:  map[string]string{"container": container, "blob": blob},
	}, nil
}

// downloader returns the blob downloader, building the real client at most once.
// When a fixed downloader was injected it is always returned.
func (p *Provider) downloader() (blobDownloader, error) {
	if p.fixed != nil {
		return p.fixed, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.built {
		return p.client, p.clientErr
	}
	p.built = true

	if p.accountURL == "" {
		p.clientErr = fmt.Errorf("azblob: no storage account configured; set %s or use WithAccountURL", AccountEnv)
		return nil, p.clientErr
	}
	cred, err := p.newCredential()
	if err != nil {
		p.clientErr = err
		return nil, err
	}
	p.client, p.clientErr = p.newClient(p.accountURL, cred)
	return p.client, p.clientErr
}

// serviceURL normalizes an account URL or bare account name to a blob service
// endpoint. A value containing "://" is used as-is (trailing slash trimmed); any
// other non-empty value is treated as an account name and expanded.
func serviceURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		return strings.TrimRight(s, "/")
	}
	return "https://" + s + ".blob.core.windows.net"
}

// sdkDownloader adapts *azblob.Client to blobDownloader, translating Azure
// not-found errors into mamori.ErrNotFound.
type sdkDownloader struct {
	client *azblobsdk.Client
}

func (d *sdkDownloader) Download(ctx context.Context, container, blob string) ([]byte, string, error) {
	resp, err := d.client.DownloadStream(ctx, container, blob, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, "", mamori.ErrNotFound
		}
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	etag := ""
	if resp.ETag != nil {
		etag = string(*resp.ETag)
	}
	if etag == "" && resp.VersionID != nil {
		etag = *resp.VersionID
	}
	return data, etag, nil
}

// isNotFound reports whether err is an Azure "blob/container does not exist"
// error (a 404). It recognizes both the typed storage error codes and a raw
// *azcore.ResponseError with a 404 status.
func isNotFound(err error) bool {
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
		return true
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}
