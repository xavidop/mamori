// Package cosmos provides a mamori Provider for Azure Cosmos DB (SQL / Core API).
//
// It resolves refs of the form:
//
//	cosmos://<database>/<container>/<id>[#field][?pk=<partitionKeyValue>]
//
// A ref names a database, a container within it, and the id of a single item.
// Resolution performs one point read (ContainerClient.ReadItem) using the item
// id and a partition key. The partition key value defaults to the item id
// (the common single-value-partition case); override it with ?pk when the
// container partitions on a different value.
//
// The item response body is the JSON document. When a #field fragment is
// present the named field is selected from that JSON with mamori.SelectKey,
// identically to every other mamori provider; otherwise the whole document JSON
// becomes Value.Bytes. A missing item (HTTP 404) is reported as an error
// satisfying errors.Is(err, mamori.ErrNotFound), so mamori can apply defaults.
//
// Value.Version is the response ETag (Cosmos returns one on every read), falling
// back to the document's own "_etag" system field, then to a content hash
// (mamori.VersionHash). Either way mamori detects changes cheaply on each poll.
// Values are not marked Sensitive by default (containers commonly hold ordinary
// config); pass WithSensitive(true) for containers that hold secret material.
//
// Cosmos DB has no cheap native push for a single item (the change feed is
// pull-based), so this provider does not implement WatchableProvider; mamori
// polls it. See the README for a note on the change feed as a future push mode.
//
// The real SDK client is built lazily on first Resolve, so construction performs
// no I/O and init-time registration is safe even without Cosmos credentials
// present. Authentication uses either the Azure default credential chain plus an
// account endpoint, or a Cosmos connection string:
//
//	// endpoint + DefaultAzureCredential
//	mamori.WithProvider(cosmos.New(cosmos.WithEndpoint("https://acct.documents.azure.com:443/")))
//
//	// connection string (AccountEndpoint=...;AccountKey=...)
//	mamori.WithProvider(cosmos.New(cosmos.WithConnectionString(cs)))
package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "cosmos"

// EndpointEnv is the environment variable consulted for the Cosmos account
// endpoint when no endpoint is supplied via options, e.g.
// https://<account>.documents.azure.com:443/. It is used together with the
// Azure default credential chain.
const EndpointEnv = "COSMOS_ENDPOINT"

// ConnectionStringEnv is the environment variable consulted for a Cosmos
// connection string when no connection string is supplied via options. When set
// it takes precedence over endpoint + credential authentication.
const ConnectionStringEnv = "COSMOS_CONNECTION_STRING"

func init() { mamori.Register(New()) }

// itemReader reads a single item from a Cosmos container by id and partition
// key. It returns the raw document bytes and a change-detection tag (the
// response ETag). It MUST return an error satisfying
// errors.Is(err, mamori.ErrNotFound) when the item does not exist. The
// production adapter wraps the azcosmos SDK; tests inject an in-memory fake.
// All parameter and result types are built-in, so callers outside this package
// can implement it too.
type itemReader interface {
	ReadItem(ctx context.Context, database, container, id, partitionKey string) (data []byte, etag string, err error)
}

// Provider resolves cosmos:// refs against a single Azure Cosmos DB account. The
// account (endpoint or connection string) is fixed for the Provider; the
// database, container, and item id come from each ref. Provider is safe for
// concurrent use.
type Provider struct {
	mu sync.Mutex

	// endpoint is the Cosmos account endpoint used with the credential chain. It
	// is ignored when connectionString is set.
	endpoint string

	// connectionString authenticates via an account key. When set it takes
	// precedence over endpoint + credential.
	connectionString string

	// sensitive marks resolved values as secret (default false).
	sensitive bool

	// fixed, when set via WithClient, is the reader used for every request (test
	// path / injected client); it bypasses credential and client building.
	fixed itemReader

	// lazily-built real reader (production path), resolved at most once.
	client    itemReader
	clientErr error
	built     bool

	// newCredential builds the token credential on first use. Overridable via
	// WithCredential; defaults to the Azure default credential chain.
	newCredential func() (azcore.TokenCredential, error)

	// newClient builds a reader from the configured auth. Overridable in tests;
	// defaults to a wrapper around the azcosmos client.
	newClient func(p *Provider, cred azcore.TokenCredential) (itemReader, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithEndpoint sets the Cosmos account endpoint (e.g.
// https://<account>.documents.azure.com:443/). It is used together with the
// Azure default credential chain, or an explicit credential from WithCredential.
func WithEndpoint(endpoint string) Option {
	return func(p *Provider) { p.endpoint = strings.TrimSpace(endpoint) }
}

// WithConnectionString authenticates with a Cosmos connection string
// (AccountEndpoint=...;AccountKey=...). When set it takes precedence over
// endpoint + credential authentication.
func WithConnectionString(cs string) Option {
	return func(p *Provider) { p.connectionString = strings.TrimSpace(cs) }
}

// WithSensitive controls whether resolved values are marked Sensitive (driving
// redaction downstream). It defaults to false; pass true for containers that
// hold secret material.
func WithSensitive(sensitive bool) Option {
	return func(p *Provider) { p.sensitive = sensitive }
}

// WithCredential injects an explicit azcore.TokenCredential (e.g. a specific
// azidentity credential) instead of the default credential chain. It has no
// effect when a connection string is configured.
func WithCredential(cred azcore.TokenCredential) Option {
	return func(p *Provider) {
		p.newCredential = func() (azcore.TokenCredential, error) { return cred, nil }
	}
}

// WithClient injects a reader used for every request, bypassing credential and
// client construction. It is primarily intended for tests (inject an in-memory
// fake), but can also supply a custom adapter.
func WithClient(c itemReader) Option {
	return func(p *Provider) { p.fixed = c }
}

// New constructs a Provider. With no options it reads the account endpoint from
// COSMOS_ENDPOINT (used with the Azure default credential chain) or a connection
// string from COSMOS_CONNECTION_STRING, and builds a real azcosmos client lazily
// on first Resolve.
//
// Users who need explicit configuration register it via
// mamori.WithProvider(cosmos.New(cosmos.WithEndpoint(...))).
func New(opts ...Option) *Provider {
	p := &Provider{
		endpoint:         strings.TrimSpace(os.Getenv(EndpointEnv)),
		connectionString: strings.TrimSpace(os.Getenv(ConnectionStringEnv)),
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
	}
	p.newClient = func(pr *Provider, cred azcore.TokenCredential) (itemReader, error) {
		if pr.connectionString != "" {
			c, err := azcosmos.NewClientFromConnectionString(pr.connectionString, nil)
			if err != nil {
				return nil, err
			}
			return &sdkReader{client: c}, nil
		}
		c, err := azcosmos.NewClient(pr.endpoint, cred, nil)
		if err != nil {
			return nil, err
		}
		return &sdkReader{client: c}, nil
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Scheme reports the URL scheme this provider handles.
func (p *Provider) Scheme() string { return Scheme }

// Resolve reads a single item from Cosmos DB. The ref path must be
// "<database>/<container>/<id>". The partition key value defaults to the id and
// is overridable with ?pk. A #field fragment selects a field from the document
// JSON. A missing item is reported as an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	database, container, id, err := splitPath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	pk := ref.Opt("pk")
	if pk == "" {
		pk = id
	}

	r, err := p.reader()
	if err != nil {
		return mamori.Value{}, fmt.Errorf("cosmos: building client: %w", err)
	}

	doc, etag, err := r.ReadItem(ctx, database, container, id, pk)
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			return mamori.Value{}, fmt.Errorf("cosmos: item %q in %s/%s not found: %w", id, database, container, mamori.ErrNotFound)
		}
		return mamori.Value{}, fmt.Errorf("cosmos: reading item %q in %s/%s: %w", id, database, container, err)
	}

	// The Version reflects the whole-document revision (the ETag is a document
	// revision), so it is derived from the raw document before any field is
	// projected: response ETag, then the document's own "_etag", then a hash.
	version := etag
	if version == "" {
		version = docETag(doc)
	}
	if version == "" {
		version = mamori.VersionHash(doc)
	}

	data := doc
	if ref.Key != "" {
		data, err = mamori.SelectKey(doc, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: p.sensitive,
		Metadata:  map[string]string{"database": database, "container": container, "id": id},
	}, nil
}

// reader returns the item reader, building the real client at most once. When a
// fixed reader was injected it is always returned.
func (p *Provider) reader() (itemReader, error) {
	if p.fixed != nil {
		return p.fixed, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.built {
		return p.client, p.clientErr
	}
	p.built = true

	if p.connectionString == "" && p.endpoint == "" {
		p.clientErr = fmt.Errorf("cosmos: no account configured; set %s or %s, or use WithEndpoint/WithConnectionString", EndpointEnv, ConnectionStringEnv)
		return nil, p.clientErr
	}

	var cred azcore.TokenCredential
	if p.connectionString == "" {
		var err error
		cred, err = p.newCredential()
		if err != nil {
			p.clientErr = err
			return nil, err
		}
	}

	p.client, p.clientErr = p.newClient(p, cred)
	return p.client, p.clientErr
}

// splitPath splits a ref path of the form "<database>/<container>/<id>" into its
// parts. The id is the remainder after the second slash and may itself contain
// slashes.
func splitPath(path string) (database, container, id string, err error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("cosmos: ref path %q must be <database>/<container>/<id>", path)
	}
	return parts[0], parts[1], parts[2], nil
}

// docETag extracts the Cosmos "_etag" system field from a document, returning ""
// when the payload is not a JSON object or lacks the field.
func docETag(doc []byte) string {
	var d struct {
		ETag string `json:"_etag"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return ""
	}
	return d.ETag
}

// sdkReader adapts an *azcosmos.Client to itemReader, translating Cosmos
// not-found errors into mamori.ErrNotFound.
type sdkReader struct {
	client *azcosmos.Client
}

func (r *sdkReader) ReadItem(ctx context.Context, database, container, id, partitionKey string) ([]byte, string, error) {
	cc, err := r.client.NewContainer(database, container)
	if err != nil {
		return nil, "", err
	}
	resp, err := cc.ReadItem(ctx, azcosmos.NewPartitionKeyString(partitionKey), id, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, "", mamori.ErrNotFound
		}
		return nil, "", err
	}
	return resp.Value, string(resp.ETag), nil
}

// isNotFound reports whether err is a Cosmos "item does not exist" error, i.e. a
// raw *azcore.ResponseError carrying HTTP 404.
func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}
