// This file exists only to prove Extract/findDerives never sees a
// WithDerive call declared in a _test.go file: Extract's loadPackages
// (../../extract.go) never sets packages.Config.Tests, matching explain's
// job of describing the shipping config surface, not the test surface - the
// same reason a source: tag declared only in a _test.go file is already
// invisible to it.
package example

import mamori "github.com/xavidop/mamori"

// DeriveTestOnly is declared only here; its WithDerive call below (see init)
// must never surface in findDerives's result, no matter how the fixture
// module's non-test packages are loaded.
type DeriveTestOnly struct {
	Hidden string
}

func init() {
	mamori.WithDerive(func(c *DeriveTestOnly) error { return nil }, "Hidden")
}
