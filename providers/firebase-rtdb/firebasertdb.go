// Package firebasertdb implements a mamori provider for the Firebase Realtime
// Database.
//
// The scheme is "firebase-rtdb" and the ref grammar is:
//
//	firebase-rtdb://<path>[#json-key]
//
// where <path> is a database location such as "config/service/db". The value at
// that path is read with the Firebase Admin SDK and its JSON becomes
// Value.Bytes. When a #json-key fragment is present the value is treated as a
// JSON object and the named field is selected via mamori.SelectKey, identically
// to every other mamori provider.
//
//	LogLevel   string `source:"firebase-rtdb://config/service/log_level"`
//	DBHost     string `source:"firebase-rtdb://config/service/db#host"`
//	DBPassword string `source:"firebase-rtdb://config/service/db#password"`
//
// The Realtime Database holds configuration, not managed secrets, so resolved
// values are not marked Sensitive. Wrap a field in secret.String if you want
// redaction anyway.
//
// # Value semantics
//
// Value.Bytes is the JSON of the value at the path; a JSON string leaf is
// returned unquoted (matching mamori.SelectKey), other JSON (objects, arrays,
// numbers, booleans) is returned as its JSON encoding. Value.Version is the
// database ETag when available (a native revision, so change detection is exact)
// and falls back to mamori.VersionHash of the payload otherwise. A null or
// missing path returns an error satisfying errors.Is(err, mamori.ErrNotFound).
//
// # Native watch
//
// The provider implements mamori.WatchableProvider using the Realtime Database
// REST streaming endpoint (Server-Sent Events). It opens
// GET <db-url>/<path>.json with Accept: text/event-stream and an ADC bearer
// token, emits the current value as a baseline, and re-resolves + emits on every
// server-pushed put/patch event. This is native push (not a polling ticker); the
// stream is bound to the watch context so cancellation aborts it and closes the
// channel without leaking goroutines.
//
// # Authentication
//
// Authentication uses Application Default Credentials (ADC): the
// GOOGLE_APPLICATION_CREDENTIALS service-account key, gcloud user credentials, or
// the workload identity / metadata server on Google infrastructure. The database
// URL is taken from WithDatabaseURL or the FIREBASE_DATABASE_URL environment
// variable. The backend is created lazily on first Resolve/Watch, so importing
// the package and registration never contact the network and never fail for lack
// of credentials.
package firebasertdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "firebase-rtdb"

// defaultReconnectBackoff is how long the watch loop waits before reconnecting a
// dropped stream (or retrying after a transient error), so a persistent failure
// does not spin.
const defaultReconnectBackoff = 2 * time.Second

// reader fetches the current value at a database path.
type reader interface {
	// Get returns the JSON-encoded value at path and its ETag. A null or missing
	// value is reported as (nil, "", nil); the provider maps that to ErrNotFound.
	Get(ctx context.Context, path string) (data []byte, etag string, err error)
}

// streamer opens a live change stream for a database path.
type streamer interface {
	// Stream opens a Server-Sent-Events stream for path. The returned stream is
	// bound to ctx: cancelling ctx unblocks Recv and terminates the stream.
	Stream(ctx context.Context, path string) (changeStream, error)
}

// changeStream is a single live Server-Sent-Events connection. Recv blocks until
// the next event, ctx cancellation, or the connection ends.
type changeStream interface {
	// Recv returns the next SSE event's name ("put", "patch", "keep-alive", ...)
	// and its raw data payload. It returns io.EOF on a clean close and ctx.Err()
	// when the bound context is cancelled.
	Recv() (event string, data []byte, err error)
	// Close releases the underlying connection. It is safe to call more than once.
	Close() error
}

// backend is the full set of Realtime Database operations the provider needs.
// The live SDK/REST backend and the in-memory test fake both satisfy it.
type backend interface {
	reader
	streamer
}

// Provider resolves firebase-rtdb:// refs against a Firebase Realtime Database.
// It is safe for concurrent use. The backend is built lazily on first use from
// Application Default Credentials and the configured database URL unless one is
// injected for testing.
type Provider struct {
	dbURL            string
	projectID        string
	reconnectBackoff time.Duration

	mu         sync.Mutex
	be         backend // resolved backend (injected or lazily built)
	newBackend func(ctx context.Context, dbURL, projectID string) (backend, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithDatabaseURL sets the Realtime Database URL, e.g.
// "https://my-project-default-rtdb.firebaseio.com". If unset, the provider falls
// back to the FIREBASE_DATABASE_URL environment variable.
func WithDatabaseURL(url string) Option {
	return func(p *Provider) { p.dbURL = url }
}

// WithProjectID sets the Google Cloud / Firebase project ID. It is optional; ADC
// usually supplies it. Provide it when the ambient credentials do not.
func WithProjectID(id string) Option {
	return func(p *Provider) { p.projectID = id }
}

// WithReconnectBackoff overrides how long Watch waits before reconnecting a
// dropped stream or retrying after a transient error (default 2s). It does not
// affect how quickly a change is observed on a healthy stream (immediate) nor how
// quickly Watch reacts to context cancellation (immediate).
func WithReconnectBackoff(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.reconnectBackoff = d
		}
	}
}

// withBackend injects a pre-built backend, bypassing lazy construction.
// Unexported: used by tests to supply an in-memory fake.
func withBackend(b backend) Option {
	return func(p *Provider) { p.be = b }
}

// New constructs a Firebase Realtime Database provider. The backend is created
// lazily on first Resolve/Watch, so New never contacts the network and never
// fails for lack of credentials or configuration.
func New(opts ...Option) *Provider {
	p := &Provider{
		reconnectBackoff: defaultReconnectBackoff,
		newBackend:       newSDKBackend,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient ADC and FIREBASE_DATABASE_URL. Users who need explicit config call
// mamori.WithProvider(firebasertdb.New(firebasertdb.WithDatabaseURL("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "firebase-rtdb".
func (p *Provider) Scheme() string { return scheme }

// getBackend returns the backing backend, building it lazily on first use.
// Concurrent callers share one backend.
func (p *Provider) getBackend(ctx context.Context) (backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.be != nil {
		return p.be, nil
	}
	if p.newBackend == nil {
		return nil, errors.New("firebase-rtdb: no backend configured")
	}
	b, err := p.newBackend(ctx, p.dbURL, p.projectID)
	if err != nil {
		return nil, fmt.Errorf("firebase-rtdb: init backend: %w", err)
	}
	p.be = b
	return b, nil
}

// Resolve fetches the current value for ref from the Realtime Database. A null or
// missing path yields an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	b, err := p.getBackend(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	data, etag, err := b.Get(ctx, ref.Path)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("firebase-rtdb: get %q: %w", ref.Path, err)
	}
	if data == nil {
		return mamori.Value{}, fmt.Errorf("firebase-rtdb: path %q: %w", ref.Path, mamori.ErrNotFound)
	}
	return valueFor(data, etag, ref)
}

// Watch implements mamori.WatchableProvider using the Realtime Database REST
// streaming endpoint. It emits the current value as a baseline, then re-resolves
// and emits on every server-pushed put/patch event. The channel is closed when
// ctx is cancelled; the goroutine never leaks because every stream Recv is bound
// to ctx.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	b, err := p.getBackend(ctx)
	if err != nil {
		return nil, err
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sleep := func(d time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(d):
				return true
			}
		}

		emittedBaseline := false
		for {
			if ctx.Err() != nil {
				return
			}
			s, err := b.Stream(ctx, ref.Path)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if !emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: stream %q: %w", ref.Path, err)}) {
					return
				}
				if !sleep(p.reconnectBackoff) {
					return
				}
				continue
			}

			// Emit the current value as a baseline once, after the stream is open
			// so that any concurrent change is captured by the live stream.
			if !emittedBaseline {
				v, rerr := p.Resolve(ctx, ref)
				if !emit(mamori.Update{Value: v, Err: rerr}) {
					_ = s.Close()
					return
				}
				emittedBaseline = true
			}

			reconnect := p.consume(ctx, s, ref, emit)
			if !reconnect {
				return
			}
			if !sleep(p.reconnectBackoff) {
				return
			}
		}
	}()
	return ch, nil
}

// consume reads events from a single stream connection until it errors or the
// context is cancelled, re-resolving and emitting on each put/patch. It always
// closes s. It returns true when the caller should reconnect (transient drop),
// false when the watch should terminate (ctx cancelled or server cancel).
func (p *Provider) consume(ctx context.Context, s changeStream, ref mamori.Ref, emit func(mamori.Update) bool) (reconnect bool) {
	defer func() { _ = s.Close() }()
	for {
		event, _, err := s.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			// Transient stream drop (EOF, network error): surface it and reconnect.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q: %w", ref.Path, err)})
			return true
		}
		switch event {
		case "put", "patch":
			// The data changed. Re-resolve to obtain a consistent value plus a
			// fresh ETag, sidestepping SSE relative-path/merge reconstruction.
			v, rerr := p.Resolve(ctx, ref)
			if ctx.Err() != nil {
				return false
			}
			// A delete surfaces as ErrNotFound; deliver it as an Update error
			// rather than terminating the watch.
			if !emit(mamori.Update{Value: v, Err: rerr}) {
				return false
			}
		case "keep-alive", "":
			// Heartbeat: no change.
		case "cancel":
			// The server ended the stream (e.g. permissions changed). Terminate.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q cancelled by server", ref.Path)})
			return false
		case "auth_revoked":
			// The auth token expired; reconnect with a fresh one.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q auth revoked", ref.Path)})
			return true
		default:
			// Unknown event type: ignore.
		}
	}
}

// valueFor converts a raw JSON value at a path into a mamori.Value, applying
// #json-key selection or scalar-string unwrapping as appropriate.
func valueFor(raw []byte, etag string, ref mamori.Ref) (mamori.Value, error) {
	b := raw
	if ref.Key != "" {
		sel, err := mamori.SelectKey(raw, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	} else {
		b = unwrapJSONString(raw)
	}
	ver := etag
	if ver == "" {
		ver = mamori.VersionHash(raw)
	}
	return mamori.Value{
		Bytes:     b,
		Version:   ver,
		Sensitive: false,
	}, nil
}

// unwrapJSONString returns the unquoted contents of a JSON string leaf, matching
// mamori.SelectKey's convention. Non-string JSON (objects, arrays, numbers,
// booleans) and non-JSON bytes are returned unchanged.
func unwrapJSONString(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return raw
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return raw
	}
	return []byte(s)
}
