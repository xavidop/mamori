// Package gcp implements a mamori provider for Google Cloud Secret Manager.
//
// It registers the "gcp-sm" scheme. Refs take the form:
//
//	gcp-sm://<project>/<secret>[#json-key][?version=<version>]
//
// where <project> is the GCP project ID (or number), <secret> is the secret ID,
// the optional #json-key selects a field from a JSON payload, and the optional
// ?version= selects a specific secret version (defaults to "latest").
//
//	DBPassword string `source:"gcp-sm://my-project/db-password"`
//	APIKey     string `source:"gcp-sm://my-project/app-config#api_key"`
//	Pinned     string `source:"gcp-sm://my-project/cert?version=3"`
//
// Authentication uses Application Default Credentials (ADC): the GOOGLE_APPLICATION_CREDENTIALS
// service-account key, gcloud user credentials, or the workload identity /
// metadata server on GCP. The underlying client is created lazily on first use,
// so registration never fails in environments without credentials.
package gcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/xavidop/mamori"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scheme is the URL scheme this provider handles.
const scheme = "gcp-sm"

// smClient is the minimal subset of the Secret Manager client used by the
// provider. The real *secretmanager.Client satisfies it; tests inject an
// in-memory fake.
type smClient interface {
	AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
	Close() error
}

// Provider resolves gcp-sm:// refs against Google Cloud Secret Manager. It is
// safe for concurrent use.
type Provider struct {
	mu     sync.Mutex
	client smClient
	// ownClient is true only when this provider built client itself, through
	// newClient. A client handed over with WithClient belongs to the caller,
	// who may still be using it elsewhere, so Close leaves it open.
	ownClient bool
	// closed records that Close ran. It is what makes Close terminal: without
	// it the nil client Close leaves behind is indistinguishable from one that
	// was never built, and the next Resolve would silently dial Secret Manager
	// on ambient credentials to rebuild what it was just told to release.
	closed bool
	// newClient builds the backing client on first use. Overridable in tests via
	// WithClient (which sets client directly) or WithClientFactory.
	newClient func(ctx context.Context) (smClient, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithClient injects a pre-built Secret Manager client. The real
// *secretmanager.Client satisfies smClient; tests pass an in-memory fake. When
// set, no lazy client is created.
//
// The client stays yours: Close leaves it open, since this provider has no way
// to know what else you built it for. Close it yourself when you are done with
// it. A client the provider builds for itself (the default, or WithClientFactory)
// is the provider's to release, and Close does release it.
func WithClient(c smClient) Option {
	return func(p *Provider) { p.client = c }
}

// WithClientFactory overrides how the backing client is lazily constructed on
// first use. It is primarily useful for advanced configuration or testing; most
// callers rely on the default (Application Default Credentials).
//
// Unlike WithClient, the client the factory returns is built on the provider's
// behalf and released by Close.
func WithClientFactory(f func(ctx context.Context) (smClient, error)) Option {
	return func(p *Provider) { p.newClient = f }
}

// New constructs a GCP Secret Manager provider. By default the underlying client
// is created lazily on first Resolve using Application Default Credentials, so
// New never contacts the network and never fails for lack of credentials.
func New(opts ...Option) *Provider {
	p := &Provider{
		newClient: func(ctx context.Context) (smClient, error) {
			return secretmanager.NewClient(ctx)
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "gcp-sm".
func (p *Provider) Scheme() string { return scheme }

// getClient returns the backing client, creating it lazily on first use. A
// closed provider is refused here, before the nil check, so no client is ever
// rebuilt after Close.
func (p *Provider) getClient(ctx context.Context) (smClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("gcp-sm: provider is closed: %w", mamori.ErrUnavailable)
	}
	if p.client != nil {
		return p.client, nil
	}
	if p.newClient == nil {
		return nil, fmt.Errorf("gcp-sm: no client and no client factory configured")
	}
	c, err := p.newClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp-sm: creating client: %w", err)
	}
	p.client = c
	p.ownClient = true
	return c, nil
}

// classifyGCP maps a gRPC status onto a mamori classification sentinel. The
// Secret Manager v1 client is gRPC-based, so codes map one to one and no
// string matching is needed.
//
// DeadlineExceeded maps to unavailable because a request that timed out is a
// backend that did not respond in time, which is what unavailable denotes.
// Codes with no clear mamori meaning (Internal, Unimplemented, Aborted) are
// returned unclassified rather than guessed at.
func classifyGCP(err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	switch status.Code(err) {
	case codes.NotFound:
		sentinel = mamori.ErrNotFound
	case codes.PermissionDenied:
		sentinel = mamori.ErrPermissionDenied
	case codes.Unauthenticated:
		sentinel = mamori.ErrUnauthenticated
	case codes.Unavailable, codes.DeadlineExceeded:
		sentinel = mamori.ErrUnavailable
	case codes.ResourceExhausted:
		sentinel = mamori.ErrRateLimited
	case codes.InvalidArgument:
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// Close releases the backing client, but only one this provider built itself. A
// client supplied with WithClient belongs to the caller and is left open;
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
	if p.client == nil || !p.ownClient {
		return nil
	}
	err := p.client.Close()
	p.client = nil
	return err
}

// Resolve fetches the secret named by ref. The ref path is <project>/<secret>;
// the version defaults to "latest" and can be overridden with ?version=. When
// ref.Key is set the JSON payload field is selected. Missing secrets/versions
// return an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}

	project, secret, ok := strings.Cut(ref.Path, "/")
	if !ok || project == "" || secret == "" {
		return mamori.Value{}, fmt.Errorf("gcp-sm: ref %q must be of the form gcp-sm://<project>/<secret>[#key][?version=]: %w", ref.Raw, mamori.ErrInvalid)
	}

	version := ref.Opt("version")
	if version == "" {
		version = "latest"
	}

	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	name := fmt.Sprintf("projects/%s/secrets/%s/versions/%s", project, secret, version)
	resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return mamori.Value{}, fmt.Errorf("gcp-sm: secret %q not found: %w: %w", ref.Path, mamori.ErrNotFound, err)
		}
		return mamori.Value{}, fmt.Errorf("gcp-sm: accessing %q: %w", ref.Path, classifyGCP(err))
	}

	data := resp.GetPayload().GetData()

	// Prefer the resolved native version name (e.g. .../versions/3) for cheap
	// change detection; fall back to a content hash if unavailable.
	ver := resp.GetName()
	if ver == "" {
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
		Sensitive: true,
	}, nil
}
