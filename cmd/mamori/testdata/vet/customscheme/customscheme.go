// Package customscheme is a vet-test fixture for the --secret-schemes flag:
// it stores a source from a custom provider ("mysecrets") in a plain string.
// The scheme is not one mamori ships, so a default run reports nothing and
// only `mamori vet --secret-schemes=mysecrets` flags it.
package customscheme

// Config binds a custom provider's secret to a plain string field. Without
// --secret-schemes this is invisible to the analyzer, which is exactly the
// gap the flag closes.
type Config struct {
	VendorToken string `source:"mysecrets://prod/vendor#token"`
}
