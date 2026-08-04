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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the URL scheme this provider handles.
const scheme = "firebase-rtdb"

// defaultReconnectBackoff is the wait before the watch loop reconnects a dropped
// stream (or retries after a transient error) for the first time, and the value
// the wait returns to once a stream has actually delivered something. It is the
// floor of an exponential backoff, not a fixed delay; see nextReconnectBackoff.
const defaultReconnectBackoff = 2 * time.Second

// reconnectBackoffCap bounds the growth of that wait, so a stream that fails the
// same way every time is retried at most this infrequently instead of forever at
// the floor. A node too large for the configured frame ceiling is the case this
// exists for: the Realtime Database opens every stream with a full put of the
// node, so such a node fails on the first frame of every connection, and a wait
// that never grew would leave the process connecting, streaming, failing and
// reconnecting for as long as the watch lives.
const reconnectBackoffCap = 30 * time.Second

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
	sse              httpcore.SSEConfig

	mu         sync.Mutex
	be         backend // resolved backend (injected or lazily built)
	newBackend func(ctx context.Context, dbURL, projectID string, sse httpcore.SSEConfig) (backend, error)
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

// WithReconnectBackoff sets the base wait before Watch reconnects a dropped
// stream or retries after a transient error (default 2s).
//
// It is the floor, not a fixed delay: each consecutive failure doubles the wait
// up to 30 seconds (or to this value, if it is larger), and the wait drops back
// to this value as soon as a stream delivers anything at all. Each wait is
// jittered to [d/2, d] so that many clients dropped by the same database do not
// reconnect in lockstep. It does not affect how quickly a change is observed on
// a healthy stream (immediate) nor how quickly Watch reacts to context
// cancellation (immediate).
func WithReconnectBackoff(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.reconnectBackoff = d
		}
	}
}

// WithMaxFrameBytes sets the ceiling on one streamed change, in bytes,
// defaulting to 1 MiB.
//
// The Realtime Database opens every stream with a put of the whole watched node,
// so this is in practice the largest node this provider can watch: a node whose
// pushed frame is over the ceiling makes Watch report an Update error and
// reconnect instead of delivering the change, for as long as the node stays that
// large. Raise it to watch a node deliberately larger than a megabyte. Resolve is
// unaffected: it reads through the Admin SDK and has never had this ceiling.
//
// Both of httpcore's bounds move together (a single line and one frame's total)
// because a put arrives as one data line, so raising only one of the two would
// still reject the node the caller raised it for. That also means the cost of
// raising it is roughly TWICE n per open watch at the moment such a frame
// arrives, since the line being read and the frame being assembled out of it
// both hold it; see httpcore.SSEConfig. Values that are not positive are
// ignored.
func WithMaxFrameBytes(n int) Option {
	return func(p *Provider) {
		if n > 0 {
			p.sse = httpcore.SSEConfig{MaxLine: n, MaxFrame: n}
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
	b, err := p.newBackend(ctx, p.dbURL, p.projectID, p.sse)
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

		// backoff is the current reconnect wait: it starts at the configured
		// floor, doubles after every attempt, and is reset to the floor by any
		// stream that delivered an event (see below).
		backoff := p.reconnectBackoff
		// wait sleeps out the current backoff, jittered, and only then grows it,
		// so the first retry waits out the configured floor rather than twice it.
		// It reports false when ctx ended during the wait, in which case the
		// caller must return instead of reconnecting.
		wait := func() bool {
			if !sleep(reconnectJitter(backoff)) {
				return false
			}
			backoff = nextReconnectBackoff(backoff, p.reconnectBackoff)
			return true
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
				// A stream that would not open delivered nothing by definition,
				// so this path only ever grows the wait; a database that is
				// refusing connections outright is the clearest case there is
				// for not asking again immediately.
				if !wait() {
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

			reconnect, delivered := p.consume(ctx, s, ref, emit)
			if !reconnect {
				return
			}
			// The reset is on a stream that DELIVERED something, not on one that
			// merely connected. Connecting proves nothing here: the Realtime
			// Database answers every stream with a put of the whole node, so a
			// node over the frame ceiling (or a server that only ever sends
			// garbage) fails on the first frame of every connection, and
			// resetting on connect would hold that failure at the floor forever.
			// A stream that ran for an hour and then dropped did deliver, so it
			// still gets the fast retry it deserves.
			if delivered {
				backoff = p.reconnectBackoff
			}
			if !wait() {
				return
			}
		}
	}()
	return ch, nil
}

// consume reads events from a single stream connection until it errors or the
// context is cancelled, re-resolving and emitting on each put/patch. It always
// closes s.
//
// reconnect is true when the caller should reconnect (transient drop), false
// when the watch should terminate (ctx cancelled or server cancel).
//
// delivered reports whether this connection produced at least one event of any
// kind, a heartbeat included. It is what tells a connection that did some work
// apart from one that failed the moment it opened, and it is the only thing the
// reconnect backoff resets on; see Watch.
func (p *Provider) consume(ctx context.Context, s changeStream, ref mamori.Ref, emit func(mamori.Update) bool) (reconnect, delivered bool) {
	defer func() { _ = s.Close() }()
	for {
		event, _, err := s.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return false, delivered
			}
			// Transient stream drop (EOF, network error): surface it and reconnect.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q: %w", ref.Path, err)})
			return true, delivered
		}
		delivered = true
		switch event {
		case "put", "patch":
			// The data changed. Re-resolve to obtain a consistent value plus a
			// fresh ETag, sidestepping SSE relative-path/merge reconstruction.
			v, rerr := p.Resolve(ctx, ref)
			if ctx.Err() != nil {
				return false, delivered
			}
			// A delete surfaces as ErrNotFound; deliver it as an Update error
			// rather than terminating the watch.
			if !emit(mamori.Update{Value: v, Err: rerr}) {
				return false, delivered
			}
		case "keep-alive", "":
			// Heartbeat: no change.
		case "cancel":
			// The server ended the stream (e.g. permissions changed). Terminate.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q cancelled by server", ref.Path)})
			return false, delivered
		case "auth_revoked":
			// The auth token expired; reconnect with a fresh one.
			emit(mamori.Update{Err: fmt.Errorf("firebase-rtdb: watch %q auth revoked", ref.Path)})
			return true, delivered
		default:
			// Unknown event type: ignore.
		}
	}
}

// nextReconnectBackoff doubles d, capped at reconnectBackoffCap or at floor,
// whichever is larger: a caller who configured a wait longer than the cap asked
// for waits at least that long, and the cap must not shorten them. The d <= 0
// check guards the (practically unreachable, given the cap) case of d doubling
// past time.Duration's maximum and wrapping negative.
func nextReconnectBackoff(d, floor time.Duration) time.Duration {
	ceiling := reconnectBackoffCap
	if floor > ceiling {
		ceiling = floor
	}
	d *= 2
	if d <= 0 || d > ceiling {
		d = ceiling
	}
	return d
}

// reconnectJitter applies "equal jitter" to d, returning a value drawn uniformly
// from [d/2, d]. This keeps the wait close to d (unlike "full jitter", [0, d])
// while stopping every client dropped by the same database at the same moment
// from reconnecting in lockstep.
func reconnectJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

// valueFor converts a raw JSON value at a path into a mamori.Value, applying
// #json-key selection or scalar-string unwrapping as appropriate.
func valueFor(raw []byte, etag string, ref mamori.Ref) (mamori.Value, error) {
	var b []byte
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
