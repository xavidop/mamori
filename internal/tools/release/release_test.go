package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const testVersion = "v0.3.0"

func cutFixture(t *testing.T, dir, commit string) (*outcome, string, error) {
	t.Helper()
	proxy := filepath.Join(t.TempDir(), "proxy")
	out, err := cut(context.Background(), options{
		Version: testVersion, Commit: commit, Repo: dir, Proxy: proxy,
		GOPROXY: "off", // the fixture has no third-party module: no network
		Log:     io.Discard,
	})
	return out, proxy, err
}

func TestCut(t *testing.T) {
	dir := newFixture(t, nil)
	base := gitIn(t, dir, "rev-parse", "HEAD")

	out, proxy, err := cutFixture(t, dir, "")
	require.NoError(t, err)

	assert.Equal(t, base, out.Base)
	assert.Equal(t, []string{testVersion, "cmd/tool/" + testVersion, "lib/" + testVersion}, out.Tags,
		"internal/tools and testdata modules are not released")
	assert.Equal(t, base, gitIn(t, dir, "rev-parse", testVersion+"^{commit}"), "the root module's tag is on the commit released")
	for _, tag := range out.Tags[1:] {
		assert.Equal(t, out.Commit, gitIn(t, dir, "rev-parse", tag+"^{commit}"), tag)
	}
	assert.Equal(t, base, gitIn(t, dir, "rev-parse", out.Commit+"^"), "the release commit sits on the commit released")
	assert.Empty(t, gitIn(t, dir, "branch", "--contains", out.Commit), "the release commit is on no branch")
	assert.Equal(t, base, gitIn(t, dir, "rev-parse", "HEAD"), "the checkout is left alone")
	assert.NotContains(t, gitIn(t, dir, "worktree", "list"), "mamori-release-", "the scratch worktree is removed")

	// The release commit changes only go.mod and go.sum files, and the
	// root module's not at all.
	changed := strings.Fields(gitIn(t, dir, "diff", "--name-only", base, out.Commit))
	assert.ElementsMatch(t, []string{"cmd/tool/go.mod", "cmd/tool/go.sum", "lib/go.mod", "lib/go.sum"}, changed)

	tool := gitIn(t, dir, "show", out.Commit+":cmd/tool/go.mod")
	assert.NotContains(t, tool, "replace")
	assert.NotContains(t, tool, "every module comes from the checkout", "a dropped replace takes its comment along")
	assert.Contains(t, tool, fixturePath+" "+testVersion+" // indirect")
	assert.Contains(t, tool, fixturePath+"/lib "+testVersion)
	lib := gitIn(t, dir, "show", out.Commit+":lib/go.mod")
	assert.NotContains(t, lib, "replace")
	assert.Contains(t, lib, "require "+fixturePath+" "+testVersion)

	// The go command, building each module from its tag the way
	// proxy.golang.org does, gets the zips the file proxy serves.
	var args []string
	for _, p := range []string{fixturePath, fixturePath + "/lib", fixturePath + "/cmd/tool"} {
		args = append(args, p+"@"+testVersion)
	}
	common := []string{"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GONOSUMDB=" + fixturePath, "GOPRIVATE=", "GONOPROXY="}
	direct := downloadSums(t, append(common, "GOPROXY=direct", "GOMODCACHE="+t.TempDir()), args)
	served := downloadSums(t, append(common, "GOPROXY="+mustFileURL(t, proxy), "GOMODCACHE="+t.TempDir()), args)
	assert.Equal(t, direct, served)
	assert.Len(t, served, 3)

	// Every go.sum has the hashes the tags serve.
	sum := gitIn(t, dir, "show", out.Commit+":cmd/tool/go.sum")
	assert.Contains(t, sum, fixturePath+"/lib "+testVersion+" "+served[fixturePath+"/lib"][0])
	assert.Contains(t, sum, fixturePath+" "+testVersion+"/go.mod "+served[fixturePath][1])

	// go install works from outside the repository.
	scratch, bin := t.TempDir(), t.TempDir()
	env := append(common, "GOPROXY="+mustFileURL(t, proxy), "GOMODCACHE="+t.TempDir(), "GOBIN="+bin, "GOFLAGS=-mod=mod -modcacherw")
	if out, err := goIn(t, scratch, env, "install", fixturePath+"/cmd/tool@"+testVersion); err != nil {
		t.Fatalf("go install: %v\n%s", err, out)
	}
	printed, err := runBinary(filepath.Join(bin, "tool"))
	require.NoError(t, err)
	assert.Equal(t, "mono/lib\n", printed)
}

func TestCutIsRepeatable(t *testing.T) {
	dir := newFixture(t, nil)
	first, _, err := cutFixture(t, dir, "")
	require.NoError(t, err)

	// The tags exist now, and a second run makes the same commit for them.
	second, _, err := cutFixture(t, dir, "")
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestCutRefusesToMoveATag(t *testing.T) {
	dir := newFixture(t, nil)
	gitIn(t, dir, "tag", "lib/"+testVersion, "HEAD")

	_, _, err := cutFixture(t, dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tag lib/"+testVersion+" exists at")
}

func TestCutReleasesTheCommitTheRootTagNames(t *testing.T) {
	dir := newFixture(t, nil)
	base := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "tag", testVersion, base)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# mono, later\n"), 0o644))
	gitIn(t, dir, "commit", "-q", "-am", "after the release")

	out, _, err := cutFixture(t, dir, "")
	require.NoError(t, err)
	assert.Equal(t, base, out.Base, "without -commit, the commit tag vX.Y.Z names")

	_, _, err = cutFixture(t, dir, "HEAD")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tag "+testVersion+" is at "+base)
}

func TestCutRefusesAThirdPartyReplace(t *testing.T) {
	dir := newFixture(t, map[string]string{
		"lib/go.mod": fixtureFiles["lib/go.mod"] + "\nreplace example.com/other => example.com/fork v1.0.0\n",
	})
	_, _, err := cutFixture(t, dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replace example.com/other => example.com/fork v1.0.0")
	assert.Empty(t, gitIn(t, dir, "tag"), "nothing is tagged")
}

func TestCutRefusesARootModuleThatRequiresASibling(t *testing.T) {
	dir := newFixture(t, map[string]string{
		"extra/go.mod": "module " + fixturePath + "/extra\n\ngo 1.27\n",
		"go.mod":       fixtureFiles["go.mod"] + "\nrequire " + fixturePath + "/extra v0.0.0\n\nreplace " + fixturePath + "/extra => ./extra\n",
	})
	_, _, err := cutFixture(t, dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the root module requires a sibling")
}

func TestCutRefusesAConvertingAttribute(t *testing.T) {
	dir := newFixture(t, map[string]string{".gitattributes": "*.go text eol=crlf\n"})
	_, _, err := cutFixture(t, dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".gitattributes:1 sets text")
}

func TestCutChecksTheVersion(t *testing.T) {
	for _, v := range []string{"1.2.3", "v1.2", "v1.2.3+build", "v2.0.0", "v9.9.9-test"} {
		_, err := cut(context.Background(), options{Version: v, Repo: t.TempDir()})
		require.Error(t, err, v)
		assert.ErrorIs(t, err, errUsage, v)
	}
}

// TestModuleZipMatchesCreateFromVCS compares moduleZip with
// golang.org/x/mod/zip's own git-based implementation, which runs git
// archive, on a tree without attributes.
func TestModuleZipMatchesCreateFromVCS(t *testing.T) {
	dir := newFixture(t, nil)
	g := repo{dir: dir}
	for _, m := range []struct{ dir, path string }{{"", fixturePath}, {"lib", fixturePath + "/lib"}, {"cmd/tool", fixturePath + "/cmd/tool"}} {
		mv := module.Version{Path: m.path, Version: testVersion}
		ours, err := moduleZip(context.Background(), g, "HEAD", m.dir, mv)
		require.NoError(t, err)
		var theirs bytes.Buffer
		require.NoError(t, modzip.CreateFromVCS(&theirs, mv, dir, "HEAD", m.dir))
		a, err := hashes(ours, nil)
		require.NoError(t, err)
		b, err := hashes(theirs.Bytes(), nil)
		require.NoError(t, err)
		assert.Equal(t, b.Zip, a.Zip, m.path)
	}

	// What the zips hold: the repository's LICENSE in every module, no
	// nested module, symlink or untracked file.
	zipData, err := moduleZip(context.Background(), g, "HEAD", "lib", module.Version{Path: fixturePath + "/lib", Version: testVersion})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"LICENSE", "go.mod", "lib.go"}, zipNames(t, zipData, fixturePath+"/lib@"+testVersion+"/"))
	zipData, err = moduleZip(context.Background(), g, "HEAD", "", module.Version{Path: fixturePath, Version: testVersion})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"LICENSE", "README.md", "go.mod", "mono.go", "scripts/x"}, zipNames(t, zipData, fixturePath+"@"+testVersion+"/"))
}

func TestPin(t *testing.T) {
	sib := siblings{"example.com/a": true, "example.com/a/b": true}
	m := &repoModule{Dir: "c", GoMod: []byte(`module example.com/a/c

go 1.27

require (
	example.com/a v0.0.0
	example.com/x v1.2.0
)

require example.com/a/b v0.0.0 // indirect

// Inside the repository, the siblings come from the checkout.
replace (
	example.com/a => ../
	example.com/a/b => ../b
)
`)}
	out, err := pin(m, sib, "v1.5.0")
	require.NoError(t, err)
	assert.Equal(t, `module example.com/a/c

go 1.27

require (
	example.com/a v1.5.0
	example.com/x v1.2.0
)

require example.com/a/b v1.5.0 // indirect
`, string(out))

	for _, unchanged := range []string{
		"module example.com/a\n\ngo 1.27\n\nrequire example.com/x v1.2.0\n",
		"module example.com/a/b\n\ngo 1.27\n\nrequire example.com/a v1.5.0\n",
	} {
		out, err := pin(&repoModule{Dir: "b", GoMod: []byte(unchanged)}, sib, "v1.5.0")
		require.NoError(t, err)
		assert.Nil(t, out, "nothing to pin: %s", unchanged)
	}

	for _, bad := range []string{
		"replace example.com/x => example.com/y v1.0.0\n",
		"replace example.com/a => example.com/fork v1.0.0\n",
		"exclude example.com/x v1.1.0\n",
	} {
		m := &repoModule{Dir: "c", GoMod: []byte("module example.com/a/c\n\ngo 1.27\n\n" + bad)}
		_, err := pin(m, sib, "v1.5.0")
		assert.Error(t, err, bad)
	}
}

func TestCheckAttributes(t *testing.T) {
	for attrs, ok := range map[string]bool{
		"*.png binary\n*.pb.go linguist-generated=true\ndocs/** export-ignore\n": true,
		"# *.go text\n*.go -text\n[attr]bin -diff -merge -text\n":                true,
		"*.go text\n":                          false,
		"*.sh eol=lf\n":                        false,
		"*.go ident\n":                         false,
		"*.bin filter=lfs diff=lfs\n":          false,
		"*.txt working-tree-encoding=UTF-16\n": false,
	} {
		dir := newFixture(t, map[string]string{"docs/.gitattributes": attrs})
		err := checkAttributes(context.Background(), repo{dir: dir}, "HEAD")
		if ok {
			assert.NoError(t, err, attrs)
		} else {
			assert.Error(t, err, attrs)
		}
	}
}

func TestCheckSums(t *testing.T) {
	want := map[string]sums{"example.com/a": {Zip: "h1:zip=", GoMod: "h1:mod="}}
	good := "example.com/a v1.0.0 h1:zip=\nexample.com/a v1.0.0/go.mod h1:mod=\nexample.com/x v1.0.0 h1:other=\nexample.com/a v0.9.0 h1:older=\n"
	assert.NoError(t, checkSums("go.sum", []byte(good), "v1.0.0", want))
	assert.ErrorContains(t, checkSums("go.sum", []byte("example.com/a v1.0.0 h1:bad=\n"), "v1.0.0", want), "go.sum:1")
	assert.ErrorContains(t, checkSums("go.sum", []byte("\nexample.com/a v1.0.0/go.mod h1:bad=\n"), "v1.0.0", want), "go.sum:2")
}

func TestOrder(t *testing.T) {
	root := &repoModule{Dir: ""}
	lib := &repoModule{Dir: "lib", Deps: []*repoModule{root}}
	app := &repoModule{Dir: "app", Deps: []*repoModule{lib, root}}
	sorted, err := order([]*repoModule{app, lib, root})
	require.NoError(t, err)
	assert.Equal(t, []*repoModule{root, lib, app}, sorted)

	lib.Deps = append(lib.Deps, app)
	_, err = order([]*repoModule{app, lib, root})
	assert.ErrorContains(t, err, "cycle")
}

func TestReleased(t *testing.T) {
	for p, want := range map[string]bool{
		"go.mod":                      true,
		"cmd/mamori/go.mod":           true,
		"internal/tools/go.mod":       false,
		"lib/testdata/fixture/go.mod": false,
		"vendor/x/go.mod":             false,
		".github/x/go.mod":            false,
		"_old/go.mod":                 false,
	} {
		assert.Equal(t, want, released(p), p)
	}
}

func TestRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 2, run(context.Background(), nil, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "-version is required")
	assert.Equal(t, 2, run(context.Background(), []string{"-version", "v2.0.0", "-repo", t.TempDir()}, &stdout, &stderr))
	assert.Equal(t, 0, run(context.Background(), []string{"-h"}, &stdout, &stderr))

	dir := newFixture(t, nil)
	stdout.Reset()
	code := run(context.Background(), []string{"-version", testVersion, "-repo", dir, "-goproxy", "off"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	assert.Equal(t, testVersion+"\ncmd/tool/"+testVersion+"\nlib/"+testVersion+"\n", stdout.String())
}

// downloadSums runs go mod download -json for args and returns each
// module's Sum and GoModSum.
func downloadSums(t *testing.T, env, args []string) map[string][2]string {
	t.Helper()
	out, err := goIn(t, t.TempDir(), env, append([]string{"mod", "download", "-json"}, args...)...)
	require.NoError(t, err, out)
	sums := map[string][2]string{}
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var m struct{ Path, Error, Sum, GoModSum string }
		require.NoError(t, dec.Decode(&m))
		require.Empty(t, m.Error, m.Path)
		sums[m.Path] = [2]string{m.Sum, m.GoModSum}
	}
	return sums
}

func mustFileURL(t *testing.T, dir string) string {
	t.Helper()
	u, err := fileURL(dir)
	require.NoError(t, err)
	return u
}
