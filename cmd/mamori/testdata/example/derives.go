// This file declares WithDerive call sites for cmd/mamori/derives_test.go's
// findDerives fixtures (see extract.go's FieldKind doc and
// cmd/mamori/derives.go's package doc for what a WithDerive-declared field
// is). Each fixture type below is scoped to exactly one derives_test.go
// test, named in its own doc comment, so a failure in one test's fixture
// never has to be disentangled from another's.
package example

import mamori "github.com/xavidop/mamori"

func init() {
	// Config.DSN is the end-to-end fixture (TestExplainListsDerivedField):
	// Config already carries several source: tagged fields (config.go);
	// DSN carries none, so the only way it can show up in `mamori explain`
	// or `mamori schema` at all is via this declared write path.
	mamori.WithDerive(func(c *Config) error { return nil }, "DSN")

	// TestFindDerivesLiteralPaths: a single literal write path.
	mamori.WithDerive(func(c *DeriveLiteral) error { return nil }, "Value")

	// TestFindDerivesMultiplePaths: one call declaring two write paths at
	// once.
	mamori.WithDerive(func(c *DeriveMulti) error { return nil }, "A", "B")

	// TestFindDerivesTwoCallsSameType: two separate calls against the same T
	// must accumulate, in call order, rather than the second overwriting the
	// first.
	mamori.WithDerive(func(c *DeriveAccumulate) error { return nil }, "X")
	mamori.WithDerive(func(c *DeriveAccumulate) error { return nil }, "Y")

	// TestFindDerivesUnknownPathDropped: "Real" names a field on
	// DeriveUnknown, "Bogus" names nothing on it. findDerives must keep the
	// former and drop the latter - a valid, deliberate WithDerive call per
	// its own doc comment (an unmatched path simply never reports as
	// written), not something the matcher failed to read, so it must not be
	// marked incomplete either.
	mamori.WithDerive(func(c *DeriveUnknown) error { return nil }, "Real", "Bogus")

	// TestFindDerivesNonLiteralMarks: the write path comes from a variable,
	// not a string literal, so findDerives cannot read it statically and
	// must mark DeriveNonLiteral incomplete instead of guessing.
	nonLiteralPaths := []string{"Computed"}
	mamori.WithDerive(func(c *DeriveNonLiteral) error { return nil }, nonLiteralPaths...)

	// TestFindDerivesIgnoresLocalFunc: this package also declares its own
	// function named WithDerive (below), unrelated to mamori's. The bare
	// call below resolves to THIS package's WithDerive, and findDerives must
	// not record anything for it - the whole point of resolving the callee
	// through types rather than name text.
	WithDerive(func(c *LocalFuncTarget) error { return nil }, "ShouldNotAppear")

	// IncompleteConfig backs TestExplainNotesIncompleteDerives (explain.go's
	// derivesIncompleteNote): it carries a real source: tagged field (so it
	// appears in Extract's output at all) plus a WithDerive call whose write
	// path is a variable, kept out of Config so this scenario can never leak
	// into TestFindDerivesLiteralPaths and friends above.
	incompletePaths := []string{"Name"}
	mamori.WithDerive(func(c *IncompleteConfig) error { return nil }, incompletePaths...)
}

// DeriveLiteral is TestFindDerivesLiteralPaths's fixture type.
type DeriveLiteral struct {
	Value string
}

// DeriveMulti is TestFindDerivesMultiplePaths's fixture type.
type DeriveMulti struct {
	A string
	B string
}

// DeriveAccumulate is TestFindDerivesTwoCallsSameType's fixture type.
type DeriveAccumulate struct {
	X string
	Y string
}

// DeriveUnknown is TestFindDerivesUnknownPathDropped's fixture type: init
// (above) declares "Real" (a real field, below) and "Bogus" (which names
// nothing on it).
type DeriveUnknown struct {
	Real string
}

// DeriveNonLiteral is TestFindDerivesNonLiteralMarks's fixture type.
type DeriveNonLiteral struct {
	Computed string
}

// LocalFuncTarget is TestFindDerivesIgnoresLocalFunc's fixture type, paired
// with this package's own WithDerive function (below), not mamori's.
type LocalFuncTarget struct {
	ShouldNotAppear string
}

// WithDerive collides in name AND generic shape with mamori.WithDerive
// (imported above and called as mamori.WithDerive elsewhere in this file):
// deliberately generic, with the identical
// func[T any](fn func(*T) error, writes ...string) signature, so that a call
// to it also populates go/types' Instances the same way a real
// mamori.WithDerive call does. Without that, the two guards in
// withDeriveTypeArg (the package-path check and the Instances lookup) would
// never be independently exercised: a non-generic decoy would already be
// rejected by "no Instances entry" alone, leaving the package-path check
// untested - see TestFindDerivesIgnoresLocalFunc, and Task 2's mandatory
// mutation-verify step for this exact guard.
func WithDerive[T any](fn func(*T) error, writes ...string) {}

// CrossTarget is TestFindDerivesCrossPackage's fixture type: it is declared
// here, in package example, but the WithDerive call naming it (see
// derivecaller/derivecaller.go) lives in a different package. findDerives
// must key its result by CrossTarget's own package (example.com/fixture),
// not derivecaller's.
type CrossTarget struct {
	Cross string
}

// IncompleteConfig's type is declared in config.go, not here - see its own
// doc comment for why (a cross-file token.Pos ordering hazard for
// taggedStructs). Its WithDerive call lives in init, above.
