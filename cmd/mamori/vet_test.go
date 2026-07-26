package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/mamori/cmd/mamori/internal/vetcheck"
)

// runVet runs vetCmd against a package under the vet fixture module
// (testdata/vet) and returns its stdout, stderr, and exit code. Like
// runExplain, it changes the working directory to the fixture module for the
// test's duration (t.Chdir restores it on cleanup), since packages.Load
// resolves patterns relative to the process's current working directory.
//
// vetCmd configures the analyzer through vetcheck.Analyzer's FlagSet, which is
// process-global, so the flag is restored after each run to keep these tests
// independent of one another and of their order.
func runVet(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := moduleRoot(t)
	t.Chdir(filepath.Join(root, "testdata", "vet"))
	resetSecretSchemesFlag(t)

	var outBuf, errBuf bytes.Buffer
	code = vetCmd(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

// resetSecretSchemesFlag restores the analyzer's -secret-schemes flag to its
// zero value when the test ends.
func resetSecretSchemesFlag(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := vetcheck.Analyzer.Flags.Set("secret-schemes", ""); err != nil {
			t.Fatalf("resetting -secret-schemes: %v", err)
		}
	})
}

// TestVetFlagsPlainSecretField checks that a package storing a secret-bearing
// source in a plain string is reported: vetCmd returns 1 and the diagnostic
// (on stderr, go vet style) names the offending field.
func TestVetFlagsPlainSecretField(t *testing.T) {
	_, stderr, code := runVet(t, "./violation")
	if code != 1 {
		t.Fatalf("vetCmd(./violation) = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "DBPassword") {
		t.Errorf("diagnostic does not name the field DBPassword:\n%s", stderr)
	}
	if !strings.Contains(stderr, "secret.String") {
		t.Errorf("diagnostic does not suggest the fix:\n%s", stderr)
	}
}

// TestVetCleanPackage checks that a package whose secret uses secret.String
// (and whose other fields use non-secret sources) reports nothing: vetCmd
// returns 0 with no diagnostics.
func TestVetCleanPackage(t *testing.T) {
	_, stderr, code := runVet(t, "./clean")
	if code != 0 {
		t.Fatalf("vetCmd(./clean) = %d, want 0; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty (no diagnostics)", stderr)
	}
}

// TestVetUnknownFlag checks that vet rejects a stray flag with a usage error
// exit code.
func TestVetUnknownFlag(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	if code := vetCmd([]string{"--nope"}, &outBuf, &errBuf); code != 1 {
		t.Errorf("vetCmd(--nope) = %d, want 1", code)
	}
}

// TestVetAnalyzesTestFiles pins mode parity: the go vet driver analyzes
// _test.go sources, so `mamori vet` must too (packages.Config{Tests: true}),
// or the two documented-equivalent modes would disagree on the same package.
func TestVetAnalyzesTestFiles(t *testing.T) {
	_, stderr, code := runVet(t, "./testonly")
	if code != 1 {
		t.Fatalf("vetCmd(./testonly) = %d, want 1 (violation lives in a _test.go); stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "TestSecret") {
		t.Errorf("diagnostic does not name the test-file field TestSecret:\n%s", stderr)
	}
}

// TestVetReportsEachFindingOnce guards against the duplicate-diagnostic risk
// that Tests:true introduces: a package with a test variant is loaded more
// than once, and its non-test violation must still be reported exactly once.
func TestVetReportsEachFindingOnce(t *testing.T) {
	_, stderr, _ := runVet(t, "./violation")
	if got := strings.Count(stderr, "has a secret-bearing source scheme"); got != 1 {
		t.Errorf("violation reported %d times, want exactly 1:\n%s", got, stderr)
	}
}

// TestVetCustomSchemeUnflaggedByDefault pins the gap that --secret-schemes
// exists to close: a custom provider's scheme is not in the built-in set, so
// a plain string holding its secret passes a default run.
func TestVetCustomSchemeUnflaggedByDefault(t *testing.T) {
	_, stderr, code := runVet(t, "./customscheme")
	if code != 0 {
		t.Fatalf("vetCmd(./customscheme) = %d, want 0 (custom scheme is unknown by default); stderr=%q", code, stderr)
	}
}

// TestVetCustomSchemeFlaggedWithFlag checks that --secret-schemes extends the
// built-in set: the same package is now reported, naming the offending field.
func TestVetCustomSchemeFlaggedWithFlag(t *testing.T) {
	_, stderr, code := runVet(t, "--secret-schemes=mysecrets", "./customscheme")
	if code != 1 {
		t.Fatalf("vetCmd(--secret-schemes=mysecrets ./customscheme) = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "VendorToken") {
		t.Errorf("diagnostic does not name the field VendorToken:\n%s", stderr)
	}
	if !strings.Contains(stderr, "mysecrets") {
		t.Errorf("diagnostic does not name the custom scheme:\n%s", stderr)
	}
}

// TestVetSecretSchemesKeepsBuiltins checks that the flag adds to the built-in
// set rather than replacing it: a built-in violation is still reported when a
// custom scheme is supplied.
func TestVetSecretSchemesKeepsBuiltins(t *testing.T) {
	_, stderr, code := runVet(t, "--secret-schemes=mysecrets", "./violation")
	if code != 1 {
		t.Fatalf("vetCmd(--secret-schemes=mysecrets ./violation) = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "DBPassword") {
		t.Errorf("built-in aws-sm scheme no longer reported:\n%s", stderr)
	}
}

// TestVetSecretSchemesForms checks the flag's accepted spellings (one or two
// dashes, "=value" or a separate argument), matching the other static
// commands' parsing.
func TestVetSecretSchemesForms(t *testing.T) {
	forms := [][]string{
		{"--secret-schemes=mysecrets"},
		{"-secret-schemes=mysecrets"},
		{"--secret-schemes", "mysecrets"},
		{"-secret-schemes", "mysecrets"},
	}
	for _, form := range forms {
		t.Run(strings.Join(form, "_"), func(t *testing.T) {
			patterns, schemes, err := parseVetArgs(append(form, "./x"))
			if err != nil {
				t.Fatalf("parseVetArgs(%v) error: %v", form, err)
			}
			if schemes != "mysecrets" {
				t.Errorf("schemes = %q, want %q", schemes, "mysecrets")
			}
			if len(patterns) != 1 || patterns[0] != "./x" {
				t.Errorf("patterns = %v, want [./x]", patterns)
			}
		})
	}
}

// TestVetSecretSchemesRejectsFullRef checks that a value that is a full ref
// rather than a bare scheme token fails immediately, instead of silently
// covering nothing. A security check that quietly does less than asked is
// worse than one that errors.
func TestVetSecretSchemesRejectsFullRef(t *testing.T) {
	if _, _, err := parseVetArgs([]string{"--secret-schemes=mysecrets://prod"}); err == nil {
		t.Fatal("parseVetArgs accepted a full ref as a scheme token, want an error")
	}

	var outBuf, errBuf bytes.Buffer
	code := vetCmd([]string{"--secret-schemes=mysecrets://prod"}, &outBuf, &errBuf)
	if code != 1 {
		t.Errorf("vetCmd with an invalid scheme = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "bare scheme token") {
		t.Errorf("error does not explain the expected form:\n%s", errBuf.String())
	}
}

// TestVetSecretSchemesMissingValue checks that the separate-argument form
// errors when the value is missing rather than swallowing a pattern.
func TestVetSecretSchemesMissingValue(t *testing.T) {
	if _, _, err := parseVetArgs([]string{"--secret-schemes"}); err == nil {
		t.Fatal("parseVetArgs(--secret-schemes) with no value = nil error, want an error")
	}
}

// TestIsVetToolInvocation pins the dual-mode seam: the go vet driver's
// invocation shapes (a single .cfg unit file, or the -flags/-V/-V=full probes)
// classify as vettool, while the CLI's own subcommands do not.
func TestIsVetToolInvocation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"cfg unit file", []string{"unit.cfg"}, true},
		{"json then cfg (real go vet unit)", []string{"-json", "unit.cfg"}, true},
		{"flags probe", []string{"-flags"}, true},
		{"version probe short", []string{"-V"}, true},
		{"version probe full", []string{"-V=full"}, true},
		{"vet subcommand", []string{"vet", "./..."}, false},
		{"explain subcommand", []string{"explain", "./..."}, false},
		{"no args", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVetToolInvocation(tt.args); got != tt.want {
				t.Errorf("isVetToolInvocation(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
