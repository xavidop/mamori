package mamori

import "context"

// Provider resolves refs of a single scheme into Values. It is the minimum a
// source must implement. Providers should be safe for concurrent use.
type Provider interface {
	// Scheme returns the URL scheme this provider handles, e.g. "aws-sm".
	Scheme() string
	// Resolve fetches the current Value for ref. It must return an error that
	// satisfies errors.Is(err, ErrNotFound) when the referenced value does not
	// exist, so defaults and optional fields can be applied.
	Resolve(ctx context.Context, ref Ref) (Value, error)
}

// WatchableProvider is an optional interface for providers with native change
// notification (Vault leases, Kubernetes informers, Consul blocking queries).
// Providers without native watch support are wrapped by mamori in a polling
// adapter - provider authors must never fake a Watch with an internal ticker.
type WatchableProvider interface {
	Provider
	// Watch returns a channel of Updates for ref. The channel is closed when the
	// watch ends (including on ctx cancellation). Transient errors are delivered
	// as Updates with a non-nil Err; channel closure signals termination.
	Watch(ctx context.Context, ref Ref) (<-chan Update, error)
}

// BatchProvider is an optional interface for providers that can resolve many
// refs in one backend call (SM BatchGetSecretValue, one file read for many
// keys). mamori uses it automatically when available.
type BatchProvider interface {
	Provider
	// ResolveBatch resolves all refs, returning a map keyed by each input Ref's
	// Raw string (equivalently Ref.String()). A ref that is not found should be
	// omitted from the map (mamori applies the default) rather than causing the
	// whole batch to fail. Ref itself is not comparable (it holds url.Values), so
	// the raw tag string is used as the key.
	ResolveBatch(ctx context.Context, refs []Ref) (map[string]Value, error)
}

// Update is a single event delivered on a WatchableProvider's channel.
type Update struct {
	// Value is the new value. Valid only when Err is nil.
	Value Value
	// Err carries a transient watch error. The channel remains open; mamori
	// surfaces the error and keeps the last-good value.
	Err error
}
