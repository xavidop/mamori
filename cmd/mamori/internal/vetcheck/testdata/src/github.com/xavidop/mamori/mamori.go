// Package mamori is a minimal stub of github.com/xavidop/mamori used only to
// compile the analysistest fixtures that call mamori.WithDerive. It mirrors
// just enough of the real signature - a generic function taking a hook and a
// variadic list of write paths, returning an Option - for the analyzer's own
// type resolution (instantiated type args, argument literals) to exercise the
// identical shape it sees in production.
package mamori

// Option is the stub of mamori's real Option, enough for analysistest.
type Option func()

// WithDerive mirrors the real signature so the analyzer's type resolution
// exercises the same shape it sees in production.
func WithDerive[T any](fn func(*T) error, writes ...string) Option {
	return func() {}
}
