// Package etcd implements a mamori provider for the etcd v3 key-value store.
//
// The scheme is "etcd" and the ref grammar is:
//
//	etcd://<key>[#json-key]
//
// The key is looked up with the etcd v3 API (clientv3). The raw value bytes
// become Value.Bytes and the key's ModRevision becomes Value.Version, giving
// cheap, native change detection. When a #json-key fragment is present the
// stored value is treated as a JSON object and the named field is selected with
// mamori.SelectKey, identically to every other provider.
//
//	DBPassword string `source:"etcd://config/app/db#password"`
//	LogLevel   string `source:"etcd://config/app/log_level"`
//
// etcd holds configuration, not managed secrets, so resolved values are not
// marked Sensitive. Wrap fields in secret.String if you want redaction anyway.
//
// The provider implements mamori.WatchableProvider using etcd's native watch
// stream: it calls Watcher.Watch(ctx, key) and emits an Update for every PUT
// event (new value + ModRevision) the server pushes. The watch channel etcd
// returns is closed when ctx is cancelled, so the provider's goroutine exits and
// the Update channel is closed with no goroutine leak.
package etcd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/xavidop/mamori"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scheme is the URL scheme this provider handles.
const scheme = "etcd"

// etcdClient is the minimal subset of *clientv3.Client the provider depends on.
// The real *clientv3.Client satisfies it directly (Get is promoted from its
// embedded KV, Watch from its embedded Watcher), and tests inject an in-memory
// fake implementing the same shape (including watch-stream semantics) so the
// conformance kit runs without a live etcd.
type etcdClient interface {
	Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan
	// Close releases the underlying client's resources (its gRPC connection,
	// watcher, and lease goroutines).
	Close() error
}

// compile-time check that the real *clientv3.Client satisfies etcdClient.
var _ etcdClient = (*clientv3.Client)(nil)

// Provider resolves etcd:// refs against an etcd v3 key-value store. It is safe
// for concurrent use. The underlying client is built lazily on first use from
// the configured endpoints (WithEndpoints or the ETCD_ENDPOINTS environment
// variable) unless a client is injected via WithClient.
type Provider struct {
	endpoints []string

	mu        sync.Mutex
	cli       etcdClient // resolved client (injected or lazily built)
	ownClient bool       // true only when this provider built cli itself via clientv3.New
	closed    bool       // Close has run; every later resolve/watch reports unavailable
}

// Option configures a Provider.
type Option func(*Provider)

// WithEndpoints sets the etcd endpoints (e.g. "localhost:2379"). It overrides
// the ETCD_ENDPOINTS environment variable.
func WithEndpoints(endpoints ...string) Option {
	return func(p *Provider) { p.endpoints = endpoints }
}

// WithClient injects a pre-configured *clientv3.Client, bypassing lazy
// construction. Use it when you build the etcd client yourself (custom TLS,
// authentication, dial options, ...).
func WithClient(c *clientv3.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.cli = c
		}
	}
}

// withClient injects a bare etcdClient. Unexported: used by tests to supply an
// in-memory fake.
func withClient(c etcdClient) Option {
	return func(p *Provider) { p.cli = c }
}

// New constructs an etcd provider. The client is created lazily on first
// Resolve/Watch, so New never fails and never contacts etcd.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient ETCD_ENDPOINTS configuration. Users who need explicit config call
// mamori.WithProvider(etcd.New(etcd.WithEndpoints("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "etcd".
func (p *Provider) Scheme() string { return scheme }

// conn returns the etcd client, building it lazily from the configured
// endpoints on first use. Concurrent callers share one client.
//
// The closed check runs first, ahead of the p.cli != nil cache check, so a
// closed provider refuses locally even when a client (injected or previously
// built) is still sitting in p.cli: Close clears p.cli, but the ordering here
// is what makes that refusal unconditional rather than incidental.
func (p *Provider) conn() (etcdClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("%w: etcd: provider is closed", mamori.ErrUnavailable)
	}
	if p.cli != nil {
		return p.cli, nil
	}
	eps := p.endpoints
	if len(eps) == 0 {
		eps = endpointsFromEnv()
	}
	if len(eps) == 0 {
		return nil, fmt.Errorf("etcd: no endpoints configured (set ETCD_ENDPOINTS or use etcd.WithEndpoints)")
	}
	c, err := clientv3.New(clientv3.Config{Endpoints: eps})
	if err != nil {
		return nil, fmt.Errorf("etcd: build client: %w", err)
	}
	p.cli = c
	p.ownClient = true
	return p.cli, nil
}

// Close releases the etcd client this provider built lazily (its gRPC
// connection and the watcher/lease goroutines built on top of it). It is a
// no-op when the client was injected by the caller with WithClient (the
// caller owns it) and when nothing was ever built, so New followed by Close
// never dials. closed is set unconditionally before either of those checks,
// so the not-owned and never-built paths still make Close terminal:
// afterwards Resolve, and any Watch STARTED after Close, report
// mamori.ErrUnavailable, refused locally by conn, rather than reaching for a
// client this provider was just told to stop using. Close is idempotent and
// safe for concurrent use.
//
// A Watch that was ALREADY RUNNING when Close is called is a different case
// and is not covered by that guarantee. Close never reaches into a running
// watch to end it (cancelling the watch's own context is the only shutdown
// path), but closing a self-dialed client does close the watch channel
// underneath it, and Watch's loop treats a closed channel as a plain return
// with no Update{Err} emitted - as does mamori's reconciler with the
// resulting closed Update channel. So that watch usually goes silently dead:
// no error reaches OnError and Watcher.Get keeps serving the last value
// indefinitely. (etcd may, less commonly, deliver one final error update
// first, if its internal watch loop exits through its error path instead.) A
// client injected with WithClient is never closed, so a watch running on one
// keeps delivering live events. Cancel the watch's context to stop a watch.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if !p.ownClient || p.cli == nil {
		return nil
	}
	err := p.cli.Close()
	p.cli = nil
	return err
}

// endpointsFromEnv parses the comma-separated ETCD_ENDPOINTS environment
// variable into a list of endpoints, trimming whitespace and dropping empties.
func endpointsFromEnv() []string {
	raw := os.Getenv("ETCD_ENDPOINTS")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// classifyEtcd maps an etcd v3 client error onto a mamori classification
// sentinel. The client is gRPC-based, so the primary signal is
// status.Code(err), exactly as classifyGCP and classifyFirestore use it.
//
// DeadlineExceeded maps to unavailable because a request that timed out is a
// backend that did not respond in time, which is what unavailable denotes.
//
// codes.InvalidArgument is deliberately left unmapped. etcd reports a bad
// username/password (rpctypes.ErrGRPCAuthFailed) as InvalidArgument, not
// Unauthenticated, which makes that code genuinely ambiguous for etcd: the
// same code also covers ordinary malformed-request errors. Mapping it to
// ErrUnauthenticated would misclassify every malformed request as an auth
// failure; mapping it to ErrInvalid would misclassify every bad-credentials
// error as a request problem. There is no way to tell the two apart from the
// code alone, so InvalidArgument stays unclassified (reports unknown) rather
// than guessed at either way.
//
// codes.NotFound is not used here either: etcd never returns it for a missing
// key. A missing key is reported as an empty Kvs slice, handled locally in
// valueFor/valueFromKV, not through an SDK error at all.
//
// The etcd v3 client has one more wrinkle beyond a plain gRPC status: for a
// fixed set of well-known server error messages (see rpctypes.errStringToError,
// which includes the exact "permission denied", "invalid auth token",
// "no leader"/"request timed out", and "database space exceeded"/"too many
// requests" texts this classifier targets), the client's internal ContextError
// helper rewrites the error into an rpctypes.EtcdError before Get/Put/Delete
// ever return it. EtcdError carries the original gRPC code via Code(), but
// does not implement the GRPCStatus() interface status.Code relies on, so
// status.Code(err) reports Unknown for it even though the code is genuinely
// known. Without the fallback below, a live server's ordinary permission,
// auth, availability, and rate-limit errors would silently report unknown
// despite being exactly what this classifier exists to catch, so
// status.Code's Unknown result is treated as inconclusive and errors.As is
// used to recover the code from rpctypes.EtcdError when present. This does
// not change the InvalidArgument decision above: ErrGRPCAuthFailed converts
// to an EtcdError the same way, and InvalidArgument still is not a case this
// switch maps.
func classifyEtcd(err error) error {
	if err == nil {
		return nil
	}
	code := status.Code(err)
	if code == codes.Unknown {
		var etcdErr rpctypes.EtcdError
		if errors.As(err, &etcdErr) {
			code = etcdErr.Code()
		}
	}
	var sentinel error
	switch code {
	case codes.PermissionDenied:
		sentinel = mamori.ErrPermissionDenied
	case codes.Unauthenticated:
		sentinel = mamori.ErrUnauthenticated
	case codes.Unavailable, codes.DeadlineExceeded:
		sentinel = mamori.ErrUnavailable
	case codes.ResourceExhausted:
		sentinel = mamori.ErrRateLimited
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// Resolve fetches the current value for ref from etcd. A missing key yields an
// error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	cli, err := p.conn()
	if err != nil {
		return mamori.Value{}, err
	}
	resp, err := cli.Get(ctx, ref.Path)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("etcd: get %q: %w", ref.Path, classifyEtcd(err))
	}
	return valueFor(resp, ref)
}

// Watch implements mamori.WatchableProvider using etcd's native watch stream. It
// subscribes with Watcher.Watch(ctx, key) and emits a fresh Update for every PUT
// event the server pushes (each carrying the new value and its ModRevision).
// etcd closes the watch channel when ctx is cancelled, so the goroutine exits
// and the Update channel is closed - no goroutine leaks.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	cli, err := p.conn()
	if err != nil {
		return nil, err
	}
	wch := cli.Watch(ctx, ref.Path)

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

		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-wch:
				if !ok {
					return
				}
				if err := resp.Err(); err != nil {
					if !emit(mamori.Update{Err: fmt.Errorf("etcd: watch %q: %w", ref.Path, classifyEtcd(err))}) {
						return
					}
					continue
				}
				for _, ev := range resp.Events {
					if ev == nil || ev.Type != clientv3.EventTypePut || ev.Kv == nil {
						continue
					}
					v, verr := valueFromKV(ev.Kv, ref)
					if !emit(mamori.Update{Value: v, Err: verr}) {
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

// valueFor converts an etcd range response into a mamori.Value, applying
// #json-key selection when requested. An empty response means the key does not
// exist.
func valueFor(resp *clientv3.GetResponse, ref mamori.Ref) (mamori.Value, error) {
	if resp == nil || len(resp.Kvs) == 0 {
		return mamori.Value{}, fmt.Errorf("etcd: key %q: %w", ref.Path, mamori.ErrNotFound)
	}
	return valueFromKV(resp.Kvs[0], ref)
}

// valueFromKV converts a single etcd key-value pair into a mamori.Value, using
// ModRevision as the native version and applying #json-key selection.
func valueFromKV(kv *mvccpb.KeyValue, ref mamori.Ref) (mamori.Value, error) {
	if kv == nil {
		return mamori.Value{}, fmt.Errorf("etcd: key %q: %w", ref.Path, mamori.ErrNotFound)
	}
	b := kv.Value
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   strconv.FormatInt(kv.ModRevision, 10),
		Sensitive: false,
	}, nil
}
