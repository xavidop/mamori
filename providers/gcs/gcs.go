// Package gcs implements a mamori provider for Google Cloud Storage objects.
//
// It registers the "gcs" scheme. Refs take the form:
//
//	gcs://<bucket>/<object>[#json-key]
//
// where <bucket> is the GCS bucket name, <object> is the object name (which may
// itself contain slashes, e.g. config/prod/app.json), and the optional
// #json-key selects a single field from a JSON-object payload.
//
//	Config    []byte `source:"gcs://my-bucket/app/config.json"`
//	FeatureX  string `source:"gcs://my-bucket/app/config.json#feature_x"`
//	Nested    string `source:"gcs://my-bucket/env/prod/settings.yaml"`
//
// Authentication uses Application Default Credentials (ADC): the
// GOOGLE_APPLICATION_CREDENTIALS service-account key, gcloud user credentials,
// or the workload identity / metadata server on GCP. The underlying client is
// created lazily on first use, so registration never fails in environments
// without credentials.
//
// Google Cloud Storage has no cheap native change notification suitable for an
// in-process watch, so this provider is intentionally NOT watchable: mamori
// polls it on the configured interval. Each poll reads the object and reports
// its generation number as Value.Version, so unchanged objects are detected
// cheaply without a byte comparison. (A future push mode could bridge Pub/Sub
// object-change notifications; see the README.)
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"github.com/xavidop/mamori"
	"google.golang.org/api/googleapi"
)

// scheme is the URL scheme this provider handles.
const scheme = "gcs"

// objectReader is the minimal "read an object" operation the provider needs.
// The real Google Cloud Storage client satisfies it through a small adapter
// (gcsClient); tests inject an in-memory fake.
//
// read returns the object's bytes, its generation number (0 if the backend does
// not supply one), and its HTTP entity tag (etag, "" if unavailable). A missing
// object MUST be signaled by an error satisfying
// errors.Is(err, storage.ErrObjectNotExist).
type objectReader interface {
	read(ctx context.Context, bucket, object string) (data []byte, generation int64, etag string, err error)
	Close() error
}

// Provider resolves gcs:// refs against Google Cloud Storage objects. It is
// safe for concurrent use.
type Provider struct {
	mu     sync.Mutex
	reader objectReader
	// ownReader is true only when this provider built reader itself, through
	// newReader. A reader handed over with WithClient belongs to the caller,
	// who may still be using it elsewhere, so Close leaves it open.
	ownReader bool
	// closed records that Close ran. It is what makes Close terminal: without
	// it the nil reader Close leaves behind is indistinguishable from one that
	// was never built, and the next Resolve would silently dial Cloud Storage
	// on ambient credentials to rebuild what it was just told to release.
	closed bool
	// newReader builds the backing client on first use. Overridable in tests via
	// WithClient (which sets reader directly) or WithClientFactory.
	newReader func(ctx context.Context) (objectReader, error)
	// sensitive marks resolved values as secret. Off by default; see WithSensitive.
	sensitive bool
}

// Option configures a Provider.
type Option func(*Provider)

// WithClient injects a pre-built object reader. The real Google Cloud Storage
// client satisfies objectReader through NewClientReader; tests pass an
// in-memory fake. When set, no lazy client is created.
//
// The reader stays yours: Close leaves it open, since this provider has no way
// to know what else you built it for. Close it yourself when you are done with
// it. A client the provider builds for itself (the default, or WithClientFactory)
// is the provider's to release, and Close does release it.
func WithClient(r objectReader) Option {
	return func(p *Provider) { p.reader = r }
}

// WithClientFactory overrides how the backing client is lazily constructed on
// first use. It is primarily useful for advanced configuration (custom
// option.ClientOptions, an emulator endpoint) or testing; most callers rely on
// the default (Application Default Credentials).
//
// Unlike WithClient, the reader the factory returns is built on the provider's
// behalf and released by Close.
func WithClientFactory(f func(ctx context.Context) (objectReader, error)) Option {
	return func(p *Provider) { p.newReader = f }
}

// WithSensitive marks every resolved value as secret (Value.Sensitive == true),
// so mamori redacts it in logs and diagnostics. Off by default, since GCS
// objects are often plain configuration; enable it for buckets that hold
// secret material.
func WithSensitive() Option {
	return func(p *Provider) { p.sensitive = true }
}

// New constructs a GCS provider. By default the underlying client is created
// lazily on first Resolve using Application Default Credentials, so New never
// contacts the network and never fails for lack of credentials.
func New(opts ...Option) *Provider {
	p := &Provider{
		newReader: func(ctx context.Context) (objectReader, error) {
			c, err := storage.NewClient(ctx)
			if err != nil {
				return nil, err
			}
			return &gcsClient{client: c}, nil
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "gcs".
func (p *Provider) Scheme() string { return scheme }

// getReader returns the backing object reader, creating it lazily on first use.
// A closed provider is refused here, before the nil check, so no client is ever
// rebuilt after Close.
func (p *Provider) getReader(ctx context.Context) (objectReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("gcs: provider is closed: %w", mamori.ErrUnavailable)
	}
	if p.reader != nil {
		return p.reader, nil
	}
	if p.newReader == nil {
		return nil, fmt.Errorf("gcs: no client and no client factory configured")
	}
	r, err := p.newReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: creating client: %w", err)
	}
	p.reader = r
	p.ownReader = true
	return r, nil
}

// Close releases the backing client, but only one this provider built itself. A
// reader supplied with WithClient belongs to the caller and is left open;
// closing it would reach outside this provider and break whatever else the
// caller is using it for.
//
// Marking the provider closed is unconditional, so Close is terminal either
// way: every later Resolve reports unavailable without touching the backend,
// whether or not there was anything here to release. Calling it more than once,
// or on a provider that never resolved, is a no-op.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.reader == nil || !p.ownReader {
		return nil
	}
	err := p.reader.Close()
	p.reader = nil
	return err
}

// classifyGCS maps a GCS object-read failure onto a mamori classification
// sentinel. The GCS Go client is REST-based, not gRPC: a missing object or
// bucket surfaces as one of the storage.Err*NotExist sentinels, and every
// other failure surfaces as a *googleapi.Error carrying the HTTP status the
// backend returned. So, unlike the gRPC-based Secret Manager client, this is a
// status-code switch, structurally the same shape as Azure Key Vault's
// classifyAzure.
//
// Only the HTTP statuses verified to actually occur for a GCS object read are
// handled: 404 (not found, alongside the storage sentinels), 403 (permission
// denied), 401 (unauthenticated), 429 (rate limited), 5xx (unavailable), and
// 400 (invalid). Any other status, and any error that is not a
// *googleapi.Error or a not-exist sentinel at all, is returned unclassified
// rather than guessed at.
func classifyGCS(err error) error {
	if err == nil {
		return nil
	}

	var sentinel error
	switch {
	case errors.Is(err, storage.ErrObjectNotExist), errors.Is(err, storage.ErrBucketNotExist):
		sentinel = mamori.ErrNotFound
	default:
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) {
			return err
		}
		switch code := gerr.Code; {
		case code == 404:
			sentinel = mamori.ErrNotFound
		case code == 403:
			sentinel = mamori.ErrPermissionDenied
		case code == 401:
			sentinel = mamori.ErrUnauthenticated
		case code == 429:
			sentinel = mamori.ErrRateLimited
		case code >= 500:
			sentinel = mamori.ErrUnavailable
		case code == 400:
			sentinel = mamori.ErrInvalid
		default:
			return err
		}
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// Resolve fetches the object named by ref. The ref path is <bucket>/<object>;
// the object name may contain slashes. When ref.Key is set the JSON payload
// field is selected. Missing objects return an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}

	bucket, object, ok := strings.Cut(ref.Path, "/")
	if !ok || bucket == "" || object == "" {
		return mamori.Value{}, fmt.Errorf("gcs: ref %q must be of the form gcs://<bucket>/<object>[#key]", ref.Raw)
	}

	r, err := p.getReader(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	data, generation, etag, err := r.read(ctx, bucket, object)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			// Double-wrap (%w: %w) rather than replacing err with the bare
			// mamori sentinel, so errors.Is(err, storage.ErrObjectNotExist)
			// keeps working for callers matching on the GCS SDK sentinel
			// directly, exactly as the README's error table promises. This
			// mirrors providers/azblob's Resolve, which preserves its
			// underlying SDK error the same way.
			return mamori.Value{}, fmt.Errorf("gcs: object %q not found: %w: %w", ref.Path, mamori.ErrNotFound, err)
		}
		return mamori.Value{}, fmt.Errorf("gcs: reading %q: %w", ref.Path, classifyGCS(err))
	}

	// Prefer the object generation for cheap change detection: it changes on
	// every overwrite. Fall back to the entity tag, then a content hash.
	var ver string
	switch {
	case generation != 0:
		ver = strconv.FormatInt(generation, 10)
	case etag != "":
		ver = etag
	default:
		ver = mamori.VersionHash(data)
	}

	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: p.sensitive,
	}, nil
}

// gcsClient adapts the real *storage.Client to the objectReader interface.
type gcsClient struct {
	client *storage.Client
}

// NewClientReader wraps a *storage.Client so it can be passed to WithClient.
// Use it when you need to build the client yourself (custom option.ClientOptions,
// a specific credentials file, an emulator endpoint):
//
//	c, err := storage.NewClient(ctx, option.WithCredentialsFile("sa.json"))
//	// ...
//	mamori.WithProvider(gcs.New(gcs.WithClient(gcs.NewClientReader(c))))
func NewClientReader(c *storage.Client) objectReader { return &gcsClient{client: c} }

func (g *gcsClient) read(ctx context.Context, bucket, object string) ([]byte, int64, string, error) {
	r, err := g.client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		// storage.ErrObjectNotExist is passed through for the caller to map.
		return nil, 0, "", err
	}
	defer func() { _ = r.Close() }()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, "", err
	}
	// ReaderObjectAttrs carries the generation but not the entity tag; the
	// generation alone is sufficient for change detection, so etag is left empty.
	return data, r.Attrs.Generation, "", nil
}

func (g *gcsClient) Close() error { return g.client.Close() }
