package main

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixturePath is the module path of the fixture repository. It is on
// github.com so that the go command, with GOPROXY=direct, fetches it with
// git from the URL https://github.com/mamori-test/mono, which the git
// configuration of the tests rewrites to the fixture's directory: no
// network, and the go command's own module zips to compare with.
const fixturePath = "github.com/mamori-test/mono"

// fixtureFiles is a small monorepo shaped like this one: a root module, a
// library and a command that require it through replace directives, a
// module the release leaves out (internal/tools), a fixture module under
// testdata, a symlink and an executable.
var fixtureFiles = map[string]string{
	"go.mod":     "module " + fixturePath + "\n\ngo 1.26\n",
	"LICENSE":    "Copyright the fixture.\n",
	"mono.go":    "// Package mono is the root module.\npackage mono\n\n// Name names it.\nconst Name = \"mono\"\n",
	"README.md":  "# mono\n",
	"scripts/x":  "#!/bin/sh\necho x\n",
	"lib/lib.go": "// Package lib is a library module.\npackage lib\n\nimport \"" + fixturePath + "\"\n\n// Name names it.\nfunc Name() string { return mono.Name + \"/lib\" }\n",
	"lib/go.mod": "module " + fixturePath + "/lib\n\ngo 1.26\n\nrequire " + fixturePath + " v0.0.0\n\n" +
		"// The checkout's root module.\nreplace " + fixturePath + " => ../\n",
	"lib/testdata/fixture/go.mod":  "module example.com/fixture\n\ngo 1.26\n",
	"lib/testdata/fixture/main.go": "package main\n\nfunc main() {}\n",
	"cmd/tool/main.go": "// Command tool prints the library's name.\npackage main\n\nimport (\n\t\"fmt\"\n\n\t\"" + fixturePath + "/lib\"\n)\n\n" +
		"func main() { fmt.Println(lib.Name()) }\n",
	"cmd/tool/go.mod": "module " + fixturePath + "/cmd/tool\n\ngo 1.26\n\nrequire (\n\t" + fixturePath + " v0.0.0 // indirect\n\t" + fixturePath + "/lib v0.0.0\n)\n\n" +
		"// Inside the repository, every module comes from the checkout.\nreplace (\n\t" + fixturePath + " => ../..\n\t" + fixturePath + "/lib => ../../lib\n)\n",
	"internal/tools/go.mod":  "module " + fixturePath + "/internal/tools\n\ngo 1.26\n",
	"internal/tools/main.go": "package main\n\nfunc main() {}\n",
}

// isolateGit gives the git commands of a test (its own and the tool's) an
// identity and a configuration of their own: no signing or hooks from the
// machine, and the github.com URL of the fixture rewritten to dir.
func isolateGit(t *testing.T, dir string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(cfg, []byte(
		"[url \""+dir+"\"]\n\tinsteadOf = https://"+fixturePath+"\n"+
			"[init]\n\tdefaultBranch = main\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.com")
	t.Setenv("GIT_AUTHOR_DATE", "1767225600 +0000")
	t.Setenv("GIT_COMMITTER_DATE", "1767225600 +0000")
	// The machine's module cache must not serve the fixture's versions.
	t.Setenv("GOMODCACHE", filepath.Join(t.TempDir(), "gomodcache"))
	t.Setenv("GOFLAGS", "-modcacherw")
}

// newFixture writes fixtureFiles (with overrides; "" removes a file) to a new
// git repository, commits them, and returns its directory.
func newFixture(t *testing.T, overrides map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	isolateGit(t, dir)
	files := map[string]string{}
	for k, v := range fixtureFiles {
		files[k] = v
	}
	for k, v := range overrides {
		files[k] = v
	}
	for name, content := range files {
		if content == "" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		require.NoError(t, os.WriteFile(p, []byte(content), mode))
	}
	require.NoError(t, os.Symlink("LICENSE", filepath.Join(dir, "LICENSE.link")))
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "the fixture")
	// Untracked: it must never reach a zip.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "untracked.go"), []byte("package lib\n"), 0o644))
	return dir
}

// gitIn runs git in dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// runBinary runs a program and returns its standard output.
func runBinary(path string, args ...string) (string, error) {
	out, err := exec.Command(path, args...).Output()
	return string(out), err
}

// zipNames lists the files of a module zip, without the module@version/
// prefix every name has.
func zipNames(t *testing.T, data []byte, prefix string) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		name, ok := strings.CutPrefix(f.Name, prefix)
		require.True(t, ok, f.Name)
		names = append(names, name)
	}
	return names
}

// goIn runs the go command in dir with env added, and returns its output.
func goIn(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}
