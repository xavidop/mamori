package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// derivesFor loads the fixture module's packages (the same t.Chdir/GOWORK=off
// pattern runExplain uses, see explain_test.go) and runs findDerives
// directly, for tests that assert on its two return values without going
// through the explain command layer.
func derivesFor(t *testing.T, patterns ...string) (map[string][]string, map[string]bool) {
	t.Helper()
	root := moduleRoot(t)
	enterFixtureModule(t, filepath.Join(root, "testdata", "example"))

	pkgs, err := loadPackages(patterns)
	if err != nil {
		t.Fatalf("loadPackages(%v): %v", patterns, err)
	}
	return findDerives(pkgs)
}

// TestFindDerivesLiteralPaths: WithDerive(fn, "DSN")-shaped call with a
// single literal path is discovered under the target type's own key.
func TestFindDerivesLiteralPaths(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	got := declared["example.com/fixture.DeriveLiteral"]
	if want := []string{"Value"}; !slices.Equal(got, want) {
		t.Errorf("declared[DeriveLiteral] = %v, want %v", got, want)
	}
}

// TestFindDerivesMultiplePaths: WithDerive(fn, "A", "B") yields both paths.
func TestFindDerivesMultiplePaths(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	got := declared["example.com/fixture.DeriveMulti"]
	if want := []string{"A", "B"}; !slices.Equal(got, want) {
		t.Errorf("declared[DeriveMulti] = %v, want %v", got, want)
	}
}

// TestFindDerivesTwoCallsSameType: two separate calls against the same T
// accumulate rather than the second overwriting the first.
func TestFindDerivesTwoCallsSameType(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	got := declared["example.com/fixture.DeriveAccumulate"]
	if want := []string{"X", "Y"}; !slices.Equal(got, want) {
		t.Errorf("declared[DeriveAccumulate] = %v, want %v (two calls must accumulate)", got, want)
	}
}

// TestFindDerivesCrossPackage: the WithDerive call lives in a different
// package (derivecaller) than its target type T (example.CrossTarget).
// findDerives must key its result by T's own package, not the call site's.
func TestFindDerivesCrossPackage(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	got := declared["example.com/fixture.CrossTarget"]
	if want := []string{"Cross"}; !slices.Equal(got, want) {
		t.Errorf("declared[CrossTarget] = %v, want %v (call site lives in a different package than T)", got, want)
	}
}

// TestFindDerivesUnknownPathDropped: a path naming no field on T ("Bogus")
// is dropped, while a real path declared in the same call ("Real") survives,
// and the type is not marked incomplete - an unmatched path is valid,
// deliberate WithDerive usage, not something the matcher failed to read.
func TestFindDerivesUnknownPathDropped(t *testing.T) {
	declared, incomplete := derivesFor(t, "./...")
	got := declared["example.com/fixture.DeriveUnknown"]
	if want := []string{"Real"}; !slices.Equal(got, want) {
		t.Errorf("declared[DeriveUnknown] = %v, want %v (Bogus names no field and must be dropped)", got, want)
	}
	if incomplete["example.com/fixture.DeriveUnknown"] {
		t.Error("DeriveUnknown marked incomplete, want false: an unknown field name is dropped, not unreadable")
	}
}

// TestFindDerivesNonLiteralMarks: WithDerive(fn, paths...) with paths a
// variable cannot be read statically, so it must set DerivesIncomplete for
// that type and must not guess at a declared path.
func TestFindDerivesNonLiteralMarks(t *testing.T) {
	declared, incomplete := derivesFor(t, "./...")
	if !incomplete["example.com/fixture.DeriveNonLiteral"] {
		t.Error("DeriveNonLiteral not marked incomplete, want true: its write paths come from a variable, not a string literal")
	}
	if got := declared["example.com/fixture.DeriveNonLiteral"]; len(got) != 0 {
		t.Errorf("declared[DeriveNonLiteral] = %v, want none: a non-literal argument cannot be read statically", got)
	}
}

// TestFindDerivesIgnoresLocalFunc: a local function named WithDerive (not
// github.com/xavidop/mamori's) must never contribute to findDerives's
// result, proving the callee is resolved through types, not name text.
func TestFindDerivesIgnoresLocalFunc(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	if got := declared["example.com/fixture.LocalFuncTarget"]; len(got) != 0 {
		t.Errorf("declared[LocalFuncTarget] = %v, want none: this package's own WithDerive must not be mistaken for mamori's", got)
	}
}

// TestFindDerivesIgnoresTestOnlyFile is Step 5's fixture: a WithDerive call
// declared only in a _test.go file must not appear, because Extract's
// loadPackages never sets packages.Config.Tests.
func TestFindDerivesIgnoresTestOnlyFile(t *testing.T) {
	declared, _ := derivesFor(t, "./...")
	if got := declared["example.com/fixture.DeriveTestOnly"]; len(got) != 0 {
		t.Errorf("declared[DeriveTestOnly] = %v, want none: a WithDerive call declared only in a _test.go file must be invisible", got)
	}
}

// TestExplainListsDerivedField is the one end-to-end check through
// runExplain: Config.DSN carries no source: tag at all, so the only way it
// can appear in `mamori explain` is via derives.go's
// mamori.WithDerive(fn, "DSN") being wired into Extract's output as a
// KindDerived field. Config's derive call is fully literal, so the
// incomplete-derives note must not print for it.
func TestExplainListsDerivedField(t *testing.T) {
	stdout, stderr, code := runExplain(t, "--type=Config", "./...")
	if code != 0 {
		t.Fatalf("explainCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	found := false
	for _, line := range strings.Split(stdout, "\n") {
		cols := strings.Fields(line)
		if len(cols) > 0 && cols[0] == "DSN" {
			found = true
			if cols[1] != "string" {
				t.Errorf("DSN TYPE column = %q, want %q", cols[1], "string")
			}
		}
	}
	if !found {
		t.Fatalf("explain table missing DSN, a WithDerive-declared field:\n%s", stdout)
	}
	if strings.Contains(stdout, "note:") {
		t.Errorf("explain printed the incomplete-derives note for Config, whose only derive call is fully literal:\n%s", stdout)
	}
}

// TestExplainNotesIncompleteDerives exercises explain.go's derivesIncompleteNote:
// IncompleteConfig's WithDerive call declares its write path via a variable
// (derives.go), so DerivesIncomplete must be true and explain must print the
// note after its table rather than silently listing an incomplete set as if
// it were complete.
func TestExplainNotesIncompleteDerives(t *testing.T) {
	stdout, stderr, code := runExplain(t, "--type=IncompleteConfig", "./...")
	if code != 0 {
		t.Fatalf("explainCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	want := "note: this struct declares WithDerive write paths that could not be read\n" +
		"      statically (a variable or a slice expansion); the derived fields listed\n" +
		"      above may be incomplete\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout missing incomplete-derives note:\n%s", stdout)
	}
}
