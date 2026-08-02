// Package derivecaller exists only to give TestFindDerivesCrossPackage (see
// ../../derives_test.go, package main) a WithDerive call site in a different
// package than its target type, example.CrossTarget (../derives.go).
// findDerives keys its result by the type argument's own package, so this
// package's import path (example.com/fixture/derivecaller) must never
// appear as a map key - example.com/fixture must.
package derivecaller

import (
	"example.com/fixture"

	mamori "github.com/xavidop/mamori"
)

func init() {
	mamori.WithDerive(func(c *example.CrossTarget) error { return nil }, "Cross")
}
