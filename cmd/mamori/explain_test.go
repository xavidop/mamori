package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update regenerates the golden files from the current explainCmd output.
// Run with: GOWORK=off go test -run TestExplain -update
var update = flag.Bool("update", false, "regenerate golden files")

// moduleRoot returns this package's directory (cmd/mamori), independent of
// any t.Chdir a test has performed, so golden file paths stay stable even
// though runExplain moves the working directory to the fixture module.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	return wd
}

// enterFixtureModule disables the Go workspace for the duration of the test
// and moves into the fixture module at dir. Every helper that runs a static
// command against a testdata fixture goes through it.
//
// The workspace part is not incidental. The fixture modules under testdata/
// are deliberately absent from the repo's go.work, the same way the Makefile
// and CI exclude testdata from module discovery. With a workspace active, go
// refuses to load a package from inside a module the workspace does not list
// ("directory prefix . does not contain modules listed in go.work"), and
// since these commands resolve patterns relative to the process's working
// directory, every fixture-based test would fail for a reason that has
// nothing to do with the code under test. That is invisible to `make test`
// and CI, which both set GOWORK=off globally, but it hits anyone running
// `go test ./...` directly from an editor.
//
// Scoping it to the test (t.Setenv restores it on cleanup) rather than
// changing the CLI keeps real behaviour untouched: a user running `mamori
// explain` inside a workspace repo generally does want workspace resolution.
func enterFixtureModule(t *testing.T, dir string) {
	t.Helper()
	// t.Setenv is incompatible with t.Parallel, which is why no fixture-based
	// test may call it. They are all fast and IO-light, so nothing is lost.
	t.Setenv("GOWORK", "off")
	t.Chdir(dir)
}

// runExplain runs explainCmd against the fixture package (testdata/example)
// and returns its stdout, stderr, and exit code. It changes the process
// working directory to the fixture module for the duration of the test
// (t.Chdir restores it automatically on cleanup), since Extract has no Dir
// parameter and golang.org/x/tools/go/packages resolves patterns relative
// to the process's current working directory.
func runExplain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := moduleRoot(t)
	enterFixtureModule(t, filepath.Join(root, "testdata", "example"))

	var outBuf, errBuf bytes.Buffer
	code = explainCmd(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

// compareGolden compares got against the golden file at
// cmd/mamori/testdata/<name>, resolved against root (captured before any
// t.Chdir). With -update it (re)writes the golden instead of comparing.
func compareGolden(t *testing.T, root, name, got string) {
	t.Helper()
	path := filepath.Join(root, "testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match golden %s\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

func TestExplainTable(t *testing.T) {
	root := moduleRoot(t)
	stdout, stderr, code := runExplain(t, "./...")
	if code != 0 {
		t.Fatalf("explainCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	compareGolden(t, root, "explain.table.golden", stdout)
}

func TestExplainJSON(t *testing.T) {
	root := moduleRoot(t)
	stdout, stderr, code := runExplain(t, "--json", "./...")
	if code != 0 {
		t.Fatalf("explainCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	compareGolden(t, root, "explain.json.golden", stdout)
}

func TestExplainTypeFilter(t *testing.T) {
	stdout, stderr, code := runExplain(t, "--type=Config", "--json", "./...")
	if code != 0 {
		t.Fatalf("explainCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	if !strings.Contains(stdout, `"TypeName": "Config"`) {
		t.Errorf("stdout missing Config struct:\n%s", stdout)
	}
	if strings.Contains(stdout, `"TypeName": "ServerConfig"`) {
		t.Errorf("stdout unexpectedly includes ServerConfig with --type=Config:\n%s", stdout)
	}
	if strings.Contains(stdout, `"TypeName": "RedisConfig"`) {
		t.Errorf("stdout unexpectedly includes standalone RedisConfig with --type=Config:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"Path": "Redis.Addr"`) {
		t.Errorf("stdout missing Config's nested Redis.Addr field:\n%s", stdout)
	}
}

// sensitiveCell returns the SENSITIVE column of the row for field in a
// `mamori explain` table, so a test can assert on one field's sensitivity
// without depending on the rest of the table's layout.
func sensitiveCell(t *testing.T, table, field string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		cols := strings.Fields(line)
		if len(cols) > 0 && cols[0] == field {
			return cols[len(cols)-1]
		}
	}
	t.Fatalf("no row for field %q in:\n%s", field, table)
	return ""
}

// TestExplainSecretSchemes checks that --secret-schemes extends the set the
// SENSITIVE column is computed from, so a custom provider's scheme can be
// reported as sensitive. The fixture's LogLevel field uses env:, which is not
// secret-bearing by default, making it a clean before/after subject.
// Each case runs in its own subtest because runExplain changes the working
// directory, which t.Chdir only restores when that (sub)test ends.
func TestExplainSecretSchemes(t *testing.T) {
	t.Run("env is not sensitive by default", func(t *testing.T) {
		stdout, stderr, code := runExplain(t, "--type=Config")
		if code != 0 {
			t.Fatalf("explainCmd() = %d, stderr = %s", code, stderr)
		}
		if got := sensitiveCell(t, stdout, "LogLevel"); got != "false" {
			t.Errorf("LogLevel SENSITIVE = %q by default, want %q", got, "false")
		}
	})

	t.Run("--secret-schemes makes it sensitive", func(t *testing.T) {
		stdout, stderr, code := runExplain(t, "--type=Config", "--secret-schemes=env")
		if code != 0 {
			t.Fatalf("explainCmd(--secret-schemes=env) = %d, stderr = %s", code, stderr)
		}
		if got := sensitiveCell(t, stdout, "LogLevel"); got != "true" {
			t.Errorf("LogLevel SENSITIVE = %q with --secret-schemes=env, want %q", got, "true")
		}
		// The built-in schemes must survive the extension, not be replaced.
		if got := sensitiveCell(t, stdout, "APIKey"); got != "true" {
			t.Errorf("APIKey SENSITIVE = %q, want the built-in aws-sm to still count", got)
		}
	})
}

// TestExplainSecretSchemesForms checks the flag's accepted spellings and that
// an invalid value (a full ref rather than a bare scheme token) is rejected
// rather than silently ignored.
func TestExplainSecretSchemesForms(t *testing.T) {
	for _, form := range [][]string{
		{"--secret-schemes=mysecrets"},
		{"-secret-schemes=mysecrets"},
		{"--secret-schemes", "mysecrets"},
		{"-secret-schemes", "mysecrets"},
	} {
		t.Run(strings.Join(form, "_"), func(t *testing.T) {
			_, _, _, schemes, err := parseExplainArgs(form)
			if err != nil {
				t.Fatalf("parseExplainArgs(%v) error: %v", form, err)
			}
			if !schemes.Contains("mysecrets") {
				t.Errorf("scheme set does not contain the added scheme: %v", schemes.Sorted())
			}
			if !schemes.Contains("vault") {
				t.Errorf("scheme set dropped the built-ins: %v", schemes.Sorted())
			}
		})
	}

	if _, _, _, _, err := parseExplainArgs([]string{"--secret-schemes=mysecrets://prod"}); err == nil {
		t.Error("parseExplainArgs accepted a full ref as a scheme token, want an error")
	}
	if _, _, _, _, err := parseExplainArgs([]string{"--secret-schemes"}); err == nil {
		t.Error("parseExplainArgs(--secret-schemes) with no value = nil error, want an error")
	}
}

// TestExplainNoSecretSchemesMeansNilSet checks that omitting the flag leaves
// the set nil, so Extract keeps using the shared built-in set rather than a
// freshly built map on every call.
func TestExplainNoSecretSchemesMeansNilSet(t *testing.T) {
	_, _, _, schemes, err := parseExplainArgs([]string{"./..."})
	if err != nil {
		t.Fatalf("parseExplainArgs error: %v", err)
	}
	if schemes != nil {
		t.Errorf("schemes = %v, want nil when --secret-schemes is absent", schemes.Sorted())
	}
}
