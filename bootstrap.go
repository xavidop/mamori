package mamori

import (
	"fmt"
	"time"
)

// DefaultBootstrapMaxAge bounds how old a restored snapshot may be while
// Health still reports the process ready. See BootstrapMaxAge.
const DefaultBootstrapMaxAge = 24 * time.Hour

// bootstrapConfig is the resolved WithBootstrapCache configuration.
//
// err carries a construction failure rather than panicking, because an Option
// cannot return one. Watch surfaces it before resolving anything.
type bootstrapConfig struct {
	path   string
	key    []byte
	maxAge time.Duration
	err    error
}

// BootstrapOption configures the bootstrap cache.
type BootstrapOption func(*bootstrapConfig)

// BootstrapMaxAge bounds how old a restored snapshot may be while Health
// reports the process ready. It defaults to DefaultBootstrapMaxAge.
//
// Inside the bound, a process that booted from a snapshot passes Health so it
// joins the load balancer, which is the point: a backend outage should not also
// be a total outage. Past it, Health fails, so a config frozen for longer than
// the operator tolerates takes the process out rather than serving indefinitely
// stale secrets.
//
// Set this to the rotation window of the shortest-lived credential in the
// config. A process serving credentials older than that will fail against the
// backend that rotated them, and failing readiness is the better outcome.
//
// Zero means unbounded. It has to be written explicitly, because a config that
// is stale forever and silent about it is not a default anyone should get by
// accident.
func BootstrapMaxAge(d time.Duration) BootstrapOption {
	return func(c *bootstrapConfig) { c.maxAge = d }
}

// WithBootstrapCache keeps an encrypted snapshot of the last known-good resolved
// values at path, and boots from it when a cold start cannot reach the backend.
//
// It is a fallback, never a fast path. Every start resolves normally first, and
// the snapshot is read only when that fails with a transient kind
// (KindUnavailable or KindRateLimited). A backend that answers and says no, a
// deleted secret or a revoked credential, fails the start: serving a cached copy
// of a value the backend deliberately removed would defeat the revocation.
//
// key must be 32 bytes; the snapshot is sealed with AES-256-GCM and the file is
// written atomically with mode 0600. Where the key comes from is a deployment
// concern, because the whole point is that mamori's own backends are unreachable
// at the moment this is needed.
//
// Enabling this creates an artifact holding live credentials at rest that did
// not exist before. That is the trade: a startup failure for a file an attacker
// with disk access and the key could read. Decide it deliberately.
func WithBootstrapCache(path string, key []byte, opts ...BootstrapOption) Option {
	c := &bootstrapConfig{path: path, maxAge: DefaultBootstrapMaxAge}
	// Copied, so a caller reusing or zeroing its buffer after this call cannot
	// change what later snapshots are sealed with.
	c.key = append([]byte(nil), key...)
	for _, opt := range opts {
		opt(c)
	}
	// Both failures are parked on c rather than returned: an Option has no error
	// to return, and Load and Watch surface this before resolving anything.
	if c.path == "" {
		c.err = fmt.Errorf("mamori: WithBootstrapCache path is required: %w", ErrInvalid)
	} else if _, err := newGCM(c.key); err != nil {
		c.err = err
	}
	return func(o *options) { o.bootstrap = c }
}
