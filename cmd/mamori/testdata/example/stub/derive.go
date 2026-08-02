// Package mamori is a minimal stub of github.com/xavidop/mamori, providing
// just enough of its WithDerive API for cmd/mamori's derives.go call-site
// matcher to have a real "github.com/xavidop/mamori.WithDerive" to resolve
// against (see config.go's package doc for why this fixture stubs core
// rather than depending on it: hermetic and fast, no real-core dependency
// tree in this fixture's go.sum).
package mamori

// Option mirrors the real package's functional-option type just enough to
// give WithDerive a return type. Nothing in this stub module ever applies an
// Option (Load/Watch are not exercised here, only the call-site shape
// WithDerive itself needs to type-check).
type Option func()

// WithDerive mirrors the real generic
// WithDerive[T any](fn func(*T) error, writes ...string) Option closely
// enough for derives.go's call-site matcher to resolve it through go/types
// exactly as it would the real package: same generic signature, same
// package path (via this module's own go.mod, replaced in from the
// fixture's go.mod).
func WithDerive[T any](fn func(*T) error, writes ...string) Option {
	return func() {}
}
