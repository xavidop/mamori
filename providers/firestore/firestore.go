// Package firestore implements a mamori provider for Google Cloud Firestore
// documents.
//
// It registers the "firestore" scheme. Refs take the form:
//
//	firestore://<collection>/<doc>[#field]
//
// where <collection> is the Firestore collection ID, <doc> is the document ID,
// and the optional #field selects a single top-level field from the document.
//
//	Config     string `source:"firestore://config/app"`         // whole document as JSON
//	LogLevel   string `source:"firestore://config/app#log_level"` // one field
//	MaxRetries int    `source:"firestore://config/app#max_retries"`
//
// Without a #field the resolved value is the document data encoded as JSON. With
// a #field the document is JSON-encoded and the named field is selected with
// mamori.SelectKey (scalars are returned unquoted; maps and arrays as their JSON
// encoding), exactly like every other mamori provider.
//
// Firestore holds application configuration, not managed secrets, so resolved
// values are not marked Sensitive. Wrap a field in secret.String for redaction.
//
// The provider implements mamori.WatchableProvider using Firestore snapshot
// listeners (DocumentRef.Snapshots): a change to the document is pushed to the
// client and delivered as an Update the instant it happens, with no polling.
//
// Authentication uses Application Default Credentials (ADC): the
// GOOGLE_APPLICATION_CREDENTIALS service-account key, gcloud user credentials,
// or the workload identity / metadata server on GCP. The project ID is detected
// from the environment unless set with WithProjectID. The underlying client is
// created lazily on first use, so registration never fails in environments
// without credentials.
package firestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	fs "cloud.google.com/go/firestore"
	"github.com/xavidop/mamori"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scheme is the URL scheme this provider handles.
const scheme = "firestore"

// snapshot is the minimal view of a Firestore document snapshot the provider
// needs. The real *firestore.DocumentSnapshot satisfies it via fsSnapshot; the
// in-memory fake used in tests implements the same shape.
type snapshot interface {
	// Exists reports whether the document exists.
	Exists() bool
	// Data returns the document's fields. It is nil for a non-existent document.
	Data() map[string]interface{}
	// UpdateTime is the time the document was last changed; the zero time when
	// unknown (the provider then falls back to a content hash for the version).
	UpdateTime() time.Time
}

// snapshotStream is the minimal view of a Firestore snapshot listener. Next
// blocks until the next snapshot of the document (the first call returns the
// current state as a baseline) and returns a non-nil error when the underlying
// context is cancelled. Stop releases the listener's resources.
type snapshotStream interface {
	Next() (snapshot, error)
	Stop()
}

// backend is the minimal Firestore surface the provider depends on. The real
// client is wrapped by fsBackend; tests inject an in-memory fake implementing
// the same interface, so the conformance kit (including native watch) runs
// without a live Firestore.
type backend interface {
	// Get fetches the document at collection/doc. A missing document is reported
	// either as a codes.NotFound error or as a snapshot whose Exists is false.
	Get(ctx context.Context, collection, doc string) (snapshot, error)
	// Snapshots opens a listener for the document at collection/doc.
	Snapshots(ctx context.Context, collection, doc string) (snapshotStream, error)
	// Close releases the backend's resources.
	Close() error
}

// Provider resolves firestore:// refs against Google Cloud Firestore. It is safe
// for concurrent use. The underlying client is built lazily on first use from
// Application Default Credentials unless one is injected via WithClient.
type Provider struct {
	projectID string

	mu      sync.Mutex
	backend backend
	// newBackend builds the backing backend on first use. Overridable in tests.
	newBackend func(ctx context.Context) (backend, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithProjectID sets the Google Cloud project ID that owns the Firestore
// database. When empty (the default) the project is detected from the ambient
// environment (GOOGLE_CLOUD_PROJECT, the credentials file, or the metadata
// server).
func WithProjectID(projectID string) Option {
	return func(p *Provider) { p.projectID = projectID }
}

// WithClient injects a pre-built *firestore.Client, bypassing lazy construction.
// Use it when you build the client yourself (custom database ID, credentials, or
// emulator endpoint).
func WithClient(c *fs.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.backend = &fsBackend{c: c}
		}
	}
}

// withBackend injects a bare backend. Unexported: used by tests to supply an
// in-memory fake.
func withBackend(b backend) Option {
	return func(p *Provider) { p.backend = b }
}

// New constructs a Firestore provider. By default the underlying client is
// created lazily on first Resolve/Watch using Application Default Credentials, so
// New never contacts the network and never fails for lack of credentials.
func New(opts ...Option) *Provider {
	p := &Provider{}
	p.newBackend = func(ctx context.Context) (backend, error) {
		projectID := p.projectID
		if projectID == "" {
			projectID = fs.DetectProjectID
		}
		c, err := fs.NewClient(ctx, projectID)
		if err != nil {
			return nil, err
		}
		return &fsBackend{c: c}, nil
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient Google Cloud configuration. Users who need explicit config call
// mamori.WithProvider(firestore.New(firestore.WithProjectID("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "firestore".
func (p *Provider) Scheme() string { return scheme }

// getBackend returns the backing backend, creating it lazily on first use.
// Concurrent callers share one backend.
func (p *Provider) getBackend(ctx context.Context) (backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.backend != nil {
		return p.backend, nil
	}
	if p.newBackend == nil {
		return nil, fmt.Errorf("firestore: no client and no client factory configured")
	}
	b, err := p.newBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("firestore: creating client: %w", err)
	}
	p.backend = b
	return b, nil
}

// Close releases the backing client, if one has been created.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.backend == nil {
		return nil
	}
	err := p.backend.Close()
	p.backend = nil
	return err
}

// splitPath splits a ref path of the form "<collection>/<doc>" into its parts.
func splitPath(ref mamori.Ref) (collection, doc string, err error) {
	collection, doc, ok := strings.Cut(ref.Path, "/")
	if !ok || collection == "" || doc == "" {
		return "", "", fmt.Errorf("firestore: ref %q must be of the form firestore://<collection>/<doc>[#field]", ref.Raw)
	}
	return collection, doc, nil
}

// Resolve fetches the document named by ref. The ref path is
// <collection>/<doc>; when ref.Key is set the named field is selected from the
// document. A missing document (or field) returns an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	collection, doc, err := splitPath(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	b, err := p.getBackend(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	snap, err := b.Get(ctx, collection, doc)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return mamori.Value{}, fmt.Errorf("firestore: document %q not found: %w", ref.Path, mamori.ErrNotFound)
		}
		return mamori.Value{}, fmt.Errorf("firestore: get %q: %w", ref.Path, err)
	}
	return valueFor(snap, ref)
}

// Watch implements mamori.WatchableProvider using a Firestore snapshot listener.
// It emits the current value as a baseline, then emits a fresh Update every time
// the document changes. The channel is closed when ctx is cancelled; the
// goroutine never leaks because the listener is bound to ctx and stopped on exit.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	collection, doc, err := splitPath(ref)
	if err != nil {
		return nil, err
	}
	b, err := p.getBackend(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := b.Snapshots(ctx, collection, doc)
	if err != nil {
		return nil, err
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer stream.Stop()

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for {
			if ctx.Err() != nil {
				return
			}
			snap, err := stream.Next()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// The listener failed for a non-cancellation reason; surface it
				// and terminate (the stream is no longer usable).
				emit(mamori.Update{Err: fmt.Errorf("firestore: watch %q: %w", ref.Path, err)})
				return
			}
			if !emit(valueUpdate(snap, ref)) {
				return
			}
		}
	}()
	return ch, nil
}

// valueUpdate turns a snapshot into a watch Update, mapping a non-existent
// document to a not-found error carried on the Update.
func valueUpdate(snap snapshot, ref mamori.Ref) mamori.Update {
	v, err := valueFor(snap, ref)
	return mamori.Update{Value: v, Err: err}
}

// valueFor converts a Firestore document snapshot into a mamori.Value, applying
// #field selection when requested. A non-existent document yields ErrNotFound.
func valueFor(snap snapshot, ref mamori.Ref) (mamori.Value, error) {
	if snap == nil || !snap.Exists() {
		return mamori.Value{}, fmt.Errorf("firestore: document %q not found: %w", ref.Path, mamori.ErrNotFound)
	}

	data, err := json.Marshal(snap.Data())
	if err != nil {
		return mamori.Value{}, fmt.Errorf("firestore: encoding document %q: %w", ref.Path, err)
	}

	// Prefer the document's native UpdateTime for cheap change detection; fall
	// back to a content hash when it is unavailable. The version is computed over
	// the whole document so a change to any field is detected even for a #field
	// ref.
	var ver string
	if ut := snap.UpdateTime(); !ut.IsZero() {
		ver = ut.UTC().Format(time.RFC3339Nano)
	} else {
		ver = mamori.VersionHash(data)
	}

	if ref.Key != "" {
		sel, err := mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: false,
	}, nil
}

// --- real Firestore adapter -------------------------------------------------

// fsBackend adapts a *firestore.Client to the backend interface.
type fsBackend struct{ c *fs.Client }

func (b *fsBackend) Get(ctx context.Context, collection, doc string) (snapshot, error) {
	snap, err := b.c.Collection(collection).Doc(doc).Get(ctx)
	if err != nil {
		// On NotFound firestore returns a non-nil snapshot (Exists()==false)
		// alongside the error; the caller detects NotFound via status.Code.
		return nil, err
	}
	return fsSnapshot{snap}, nil
}

func (b *fsBackend) Snapshots(ctx context.Context, collection, doc string) (snapshotStream, error) {
	it := b.c.Collection(collection).Doc(doc).Snapshots(ctx)
	return &fsStream{it: it}, nil
}

func (b *fsBackend) Close() error { return b.c.Close() }

// fsSnapshot adapts *firestore.DocumentSnapshot to the snapshot interface.
type fsSnapshot struct{ s *fs.DocumentSnapshot }

func (s fsSnapshot) Exists() bool                 { return s.s.Exists() }
func (s fsSnapshot) Data() map[string]interface{} { return s.s.Data() }
func (s fsSnapshot) UpdateTime() time.Time        { return s.s.UpdateTime }

// fsStream adapts *firestore.DocumentSnapshotIterator to snapshotStream.
type fsStream struct{ it *fs.DocumentSnapshotIterator }

func (s *fsStream) Next() (snapshot, error) {
	snap, err := s.it.Next()
	if err != nil {
		return nil, err
	}
	return fsSnapshot{snap}, nil
}

func (s *fsStream) Stop() { s.it.Stop() }

// Compile-time checks that the adapters satisfy the interfaces.
var (
	_ backend        = (*fsBackend)(nil)
	_ snapshot       = fsSnapshot{}
	_ snapshotStream = (*fsStream)(nil)
)
