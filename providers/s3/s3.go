// Package s3 provides a mamori value provider backed by Amazon S3 and any
// S3-compatible object store (MinIO, Cloudflare R2, Backblaze B2, Ceph, ...).
//
// It registers a single scheme:
//
//	s3://<bucket>/<key>[#json-key]
//
// <key> is an object key and may itself contain slashes, because object keys
// are paths: everything after the first "/" (the bucket segment) is the object
// key. A #json-key fragment selects a single field from a JSON object payload
// using mamori.SelectKey, identically to every other mamori provider:
//
//	s3://my-bucket/config/app.json          # the whole object
//	s3://my-bucket/config/app.json#database # one field of a JSON object
//	s3://my-bucket/secrets/tls.pem          # a nested key with slashes
//
// The underlying AWS SDK client is created lazily on first Resolve using the
// default AWS credential chain (environment, shared config/profile,
// EC2/ECS/EKS role, SSO, ...), so construction never performs I/O and never
// fails. Region and endpoint may be overridden with WithRegion and
// WithEndpoint; the latter targets MinIO, Cloudflare R2, or any custom
// S3-compatible endpoint (path-style addressing is enabled automatically when
// a custom endpoint is set).
//
// Value.Version is the object ETag (surrounding quotes stripped) when present,
// or the S3 VersionId when the object carries one, else mamori.VersionHash of
// the payload. S3 has no cheap native change push, so this provider does NOT
// implement mamori.WatchableProvider - mamori polls it, using the ETag Version
// for cheap change detection (an unchanged ETag means an unchanged object, with
// no body transfer needed on the caller's side). A future push mode could
// bridge S3 Event Notifications through SQS or EventBridge.
//
// Objects are not marked Sensitive by default. Because S3 buckets frequently
// hold secret bundles - JSON credential documents, PEM chains, dotenv files -
// pass WithSensitive(true) to mark every resolved value as secret so it is
// redacted downstream.
//
// Usage with ambient credentials is automatic via the registered provider.
// Callers who need explicit configuration use:
//
//	cfg, _ := mamori.Load[Config](ctx,
//	    mamori.WithProvider(s3.New(
//	        s3.WithRegion("us-east-1"),
//	        s3.WithSensitive(true),
//	    )),
//	)
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme handled by Provider.
const scheme = "s3"

// s3API is the minimal subset of the S3 client Provider uses. The real
// *s3.Client satisfies it; tests inject an in-memory fake.
type s3API interface {
	GetObject(ctx context.Context, params *awss3.GetObjectInput, optFns ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
}

// options holds the construction-time configuration for a Provider.
type options struct {
	region    string
	endpoint  string
	sensitive bool
}

// Option customizes a Provider constructed with New.
type Option func(*options)

// WithRegion pins the AWS region for the provider's client. When unset, the
// region is resolved from the ambient AWS configuration (AWS_REGION,
// AWS_DEFAULT_REGION, the shared config file, or instance metadata). Custom
// S3-compatible endpoints still require a region for request signing (for
// Cloudflare R2 use "auto").
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// WithEndpoint overrides the S3 endpoint URL, targeting an S3-compatible store
// such as MinIO (e.g. "http://localhost:9000"), Cloudflare R2
// (e.g. "https://<accountid>.r2.cloudflarestorage.com"), or Backblaze B2. When
// set, path-style addressing is enabled automatically so bucket names are not
// folded into the host.
func WithEndpoint(endpoint string) Option {
	return func(o *options) { o.endpoint = endpoint }
}

// WithSensitive marks every resolved value as Sensitive (secret) so it is
// redacted downstream. It is off by default; enable it when the bucket holds
// secret material.
func WithSensitive(sensitive bool) Option {
	return func(o *options) { o.sensitive = sensitive }
}

func newOptions(opts []Option) options {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Provider resolves s3://<bucket>/<key>[#json-key] refs against Amazon S3 or an
// S3-compatible object store. The zero-effort path uses the default AWS
// credential chain. It is safe for concurrent use.
type Provider struct {
	opts   options
	mu     sync.Mutex
	client s3API
}

// Compile-time interface check. The provider deliberately does NOT implement
// mamori.WatchableProvider: S3 has no cheap native change push, so mamori polls
// it using the ETag Version.
var _ mamori.Provider = (*Provider)(nil)

// New constructs an S3 provider. The underlying AWS client is built lazily on
// first Resolve using the default credential chain, so construction never
// performs I/O and never fails.
func New(opts ...Option) *Provider {
	return &Provider{opts: newOptions(opts)}
}

// newWithClient returns a provider backed by a caller-supplied client. It is the
// injection seam used by tests to supply an in-memory fake.
func newWithClient(c s3API, opts ...Option) *Provider {
	return &Provider{client: c, opts: newOptions(opts)}
}

// Scheme returns "s3".
func (p *Provider) Scheme() string { return scheme }

// getClient returns the cached client, building the real one on first use.
func (p *Provider) getClient(ctx context.Context) (s3API, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if p.opts.region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(p.opts.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}
	p.client = awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if p.opts.endpoint != "" {
			o.BaseEndpoint = awssdk.String(p.opts.endpoint)
			o.UsePathStyle = true // MinIO / R2 / custom stores generally need path-style
		}
	})
	return p.client, nil
}

// Resolve fetches the current value of a single object via GetObject. When
// ref.Key is set and the payload is a JSON object, the named field is selected.
// A missing object or bucket is reported as an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	bucket, key, err := splitBucketKey(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	out, err := client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: awssdk.String(bucket),
		Key:    awssdk.String(key),
	})
	if err != nil {
		return mamori.Value{}, mapError(ref, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("s3: read object %q: %w", ref.Path, err)
	}
	return p.value(ref.Key, data, out.ETag, out.VersionId)
}

// value assembles a mamori.Value from an object's raw bytes and revision
// metadata. The Version is computed from the whole object (ETag, then VersionId,
// then a content hash) BEFORE any #json-key selection, so it tracks changes to
// the underlying object regardless of which field is selected.
func (p *Provider) value(key string, data []byte, etag, versionID *string) (mamori.Value, error) {
	version := strings.Trim(awssdk.ToString(etag), `"`)
	if version == "" {
		version = awssdk.ToString(versionID)
	}
	if key != "" {
		sel, err := mamori.SelectKey(data, key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}
	if version == "" {
		version = mamori.VersionHash(data)
	}
	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: p.opts.sensitive,
	}, nil
}

// splitBucketKey splits a ref path of the form "<bucket>/<key...>" into its
// bucket and object key. The key may contain further slashes. Both parts are
// required.
func splitBucketKey(path string) (bucket, key string, err error) {
	bucket, key, ok := strings.Cut(path, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("s3: ref %q must be of the form s3://<bucket>/<key>", path)
	}
	return bucket, key, nil
}

// mapError maps an S3 not-found error (a missing object or bucket) to
// mamori.ErrNotFound and otherwise annotates the error with the ref for
// diagnostics. Typed NoSuchKey/NoSuchBucket errors are matched first; a generic
// smithy APIError with a NotFound/404 code is also treated as not-found so that
// S3-compatible stores (MinIO, R2) that do not return the exact typed shapes
// still yield ErrNotFound.
func mapError(ref mamori.Ref, err error) error {
	var noKey *s3types.NoSuchKey
	var noBucket *s3types.NoSuchBucket
	if errors.As(err, &noKey) || errors.As(err, &noBucket) {
		return fmt.Errorf("s3: object %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return fmt.Errorf("s3: object %q not found: %w", ref.Path, mamori.ErrNotFound)
		}
	}
	return fmt.Errorf("s3: resolve %q: %w", ref.Path, err)
}

// init registers a lazily-initialized provider so that s3:// refs resolve out of
// the box under ambient AWS credentials.
func init() {
	mamori.Register(New())
}
