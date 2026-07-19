// Package middleware provides composable Provider decorators: Cache, Audit,
// Failover, RateLimit, and Prefix. Because every provider implements the same
// mamori.Provider interface, middleware nests freely:
//
//	mamori.WithProvider(
//	    middleware.Cache(5*time.Minute,
//	        middleware.Audit(logger,
//	            middleware.Failover(primary, replica))))
//
// Middleware instruments the Resolve path. When the wrapped provider supports
// native watching, Watch is delegated to it (with any ref rewriting applied);
// otherwise mamori's poller drives Resolve through the middleware.
package middleware

import (
	"context"
	"errors"

	"github.com/xavidop/mamori"
)

// errNotWatchable is returned by a wrapper's Watch when the inner provider has no
// native watch support, signalling mamori to fall back to polling.
var errNotWatchable = errors.New("mamori/middleware: inner provider is not watchable")

// wrapper is the shared decorator implementation. It always advertises Provider,
// WatchableProvider, and BatchProvider; Watch and ResolveBatch delegate or fall
// back so capabilities are preserved without per-middleware boilerplate.
type wrapper struct {
	inner   mamori.Provider
	scheme  string
	resolve func(ctx context.Context, ref mamori.Ref) (mamori.Value, error)
	mapRef  func(ref mamori.Ref) mamori.Ref
}

func newWrapper(inner mamori.Provider, resolve func(context.Context, mamori.Ref) (mamori.Value, error), mapRef func(mamori.Ref) mamori.Ref) *wrapper {
	if mapRef == nil {
		mapRef = func(r mamori.Ref) mamori.Ref { return r }
	}
	return &wrapper{inner: inner, scheme: inner.Scheme(), resolve: resolve, mapRef: mapRef}
}

func (w *wrapper) Scheme() string { return w.scheme }

func (w *wrapper) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	return w.resolve(ctx, ref)
}

// Watch delegates to the inner provider's native watch (after ref rewriting) when
// available; otherwise it returns errNotWatchable so mamori polls instead.
func (w *wrapper) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	if wp, ok := w.inner.(mamori.WatchableProvider); ok {
		return wp.Watch(ctx, w.mapRef(ref))
	}
	return nil, errNotWatchable
}

// ResolveBatch loops through the middleware's Resolve so decoration still applies
// to each ref. (A batch-native inner is still reached because each Resolve call
// flows into the wrapped chain.)
func (w *wrapper) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	out := make(map[string]mamori.Value, len(refs))
	for _, ref := range refs {
		v, err := w.resolve(ctx, ref)
		if err != nil {
			if errors.Is(err, mamori.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out[ref.Raw] = v
	}
	return out, nil
}
