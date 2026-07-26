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

// runExplain runs explainCmd against the fixture package (testdata/example)
// and returns its stdout, stderr, and exit code. It changes the process
// working directory to the fixture module for the duration of the test
// (t.Chdir restores it automatically on cleanup), since Extract has no Dir
// parameter and golang.org/x/tools/go/packages resolves patterns relative
// to the process's current working directory.
func runExplain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := moduleRoot(t)
	t.Chdir(filepath.Join(root, "testdata", "example"))

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
