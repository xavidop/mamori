package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// options of one release.
type options struct {
	Version string    // vX.Y.Z
	Commit  string    // the commit to release; "" for the commit tag Version names, or HEAD
	Repo    string    // a directory of the repository
	Proxy   string    // the directory of the file proxy; "" for a temporary one
	GOPROXY string    // where third-party modules come from, after the module cache
	Log     io.Writer // progress
}

// outcome of a release.
type outcome struct {
	Base   string   // the commit released, which the root module's tag names
	Commit string   // the release commit, which the other modules' tags name
	Tags   []string // every tag of the release, the root module's first
}

// checkVersion accepts the versions a module without a /vN suffix can have.
func checkVersion(v string) error {
	if !semver.IsValid(v) || semver.Canonical(v) != v {
		return fmt.Errorf("%w: %q is not a version vX.Y.Z", errUsage, v)
	}
	if m := semver.Major(v); m != "v0" && m != "v1" {
		return fmt.Errorf("%w: %s needs module paths ending in /%s, which these modules do not have", errUsage, v, m)
	}
	return nil
}

// cut makes the release: the release commit, the file proxy and the tags.
func cut(ctx context.Context, o options) (*outcome, error) {
	if err := checkVersion(o.Version); err != nil {
		return nil, err
	}
	logf := func(format string, args ...any) {
		if o.Log != nil {
			_, _ = fmt.Fprintf(o.Log, format+"\n", args...)
		}
	}
	top, err := repo{dir: o.Repo}.output(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	g := repo{dir: top}

	rootTag, err := g.tagCommit(ctx, o.Version)
	if err != nil {
		return nil, err
	}
	rev := o.Commit
	if rev == "" {
		rev = "HEAD"
		if rootTag != "" {
			rev = o.Version
		}
	}
	base, err := g.commitOf(ctx, rev)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a commit of %s", errUsage, rev, top)
	}
	if rootTag != "" && rootTag != base {
		return nil, fmt.Errorf("tag %s is at %s, not at %s (%s)", o.Version, rootTag, base, rev)
	}

	if err := checkAttributes(ctx, g, base); err != nil {
		return nil, err
	}
	mods, err := discover(ctx, g, base)
	if err != nil {
		return nil, err
	}
	sib := siblingsOf(mods)
	pinned := map[*repoModule][]byte{}
	for _, m := range mods {
		data, err := pin(m, sib, o.Version)
		if err != nil {
			return nil, err
		}
		if data != nil {
			pinned[m] = data
		}
	}
	if _, ok := pinned[rootOf(mods)]; ok {
		return nil, fmt.Errorf("the root module requires a sibling or replaces one: its go.mod would change, but its tag %s stays on %s", o.Version, base)
	}
	logf("release: %s of %s, %d modules", o.Version, base, len(mods))

	tmp, err := os.MkdirTemp("", "mamori-release-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // GOFLAGS=-modcacherw keeps its module cache removable
	proxy := o.Proxy
	if proxy == "" {
		proxy = filepath.Join(tmp, "proxy")
	}
	if proxy, err = filepath.Abs(proxy); err != nil {
		return nil, err
	}
	env, err := goEnv(proxy, filepath.Join(tmp, "gomodcache"), o.GOPROXY, rootOf(mods).Path)
	if err != nil {
		return nil, err
	}

	// A scratch worktree of the commit: only committed files reach a zip,
	// and the checkout the tool runs in is left alone.
	wtDir := filepath.Join(tmp, "tree")
	if _, err := g.output(ctx, "worktree", "add", "--detach", "--quiet", wtDir, base); err != nil {
		return nil, err
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_, _ = g.output(ctx, "worktree", "remove", "--force", wtDir)
		_, _ = g.output(ctx, "worktree", "prune")
	}()
	wt := repo{dir: wtDir}

	ident, when, err := identity(ctx, g, base)
	if err != nil {
		return nil, err
	}

	// In dependency order: a module's go.sum can only be completed once the
	// proxy has the zips of the siblings it requires, and its own zip, which
	// holds its go.sum, only once that is done.
	made := map[string]sums{}
	for _, m := range mods {
		mv := module.Version{Path: m.Path, Version: o.Version}
		if data, ok := pinned[m]; ok {
			logf("==> %s: go mod tidy", m)
			dir := filepath.Join(wtDir, filepath.FromSlash(m.Dir))
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), data, 0o644); err != nil {
				return nil, err
			}
			if err := goModTidy(ctx, dir, env); err != nil {
				return nil, err
			}
			tidied, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err != nil {
				return nil, err
			}
			if err := checkPinned(path.Join(m.Dir, "go.mod"), tidied, sib, o.Version); err != nil {
				return nil, err
			}
			add := []string{"add", "--", path.Join(m.Dir, "go.mod")}
			if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
				add = append(add, path.Join(m.Dir, "go.sum"))
			}
			if _, err := wt.output(ctx, add...); err != nil {
				return nil, err
			}
		}
		tree, err := wt.output(ctx, "write-tree")
		if err != nil {
			return nil, err
		}
		zipData, gomod, err := moduleFiles(ctx, wt, tree, m, mv)
		if err != nil {
			return nil, err
		}
		s, err := hashes(zipData, gomod)
		if err != nil {
			return nil, err
		}
		made[m.Path] = s
		if err := addToProxy(proxy, mv, when, gomod, zipData); err != nil {
			return nil, err
		}
	}

	tree, err := wt.output(ctx, "write-tree")
	if err != nil {
		return nil, err
	}
	baseTree, err := g.output(ctx, "rev-parse", base+"^{tree}")
	if err != nil {
		return nil, err
	}
	commit := base
	if tree != baseTree {
		c := repo{dir: top, env: ident}
		commit, err = c.output(ctx, "commit-tree", "--no-gpg-sign", "-p", base, "-m", message(o.Version), tree)
		if err != nil {
			return nil, err
		}
	}
	logf("release: release commit %s", commit)

	// What each tag will serve is what go mod tidy used, and every go.sum
	// has its hashes.
	for _, m := range mods {
		mv := module.Version{Path: m.Path, Version: o.Version}
		zipData, gomod, err := moduleFiles(ctx, g, tagged(m, base, commit), m, mv)
		if err != nil {
			return nil, err
		}
		s, err := hashes(zipData, gomod)
		if err != nil {
			return nil, err
		}
		if s != made[m.Path] {
			return nil, fmt.Errorf("%s: the zip of tag %s (%s) is not the one the release was tidied with (%s)", m, m.Tag(o.Version), s.Zip, made[m.Path].Zip)
		}
		gosum, ok, err := g.file(ctx, commit, path.Join(m.Dir, "go.sum"))
		if err != nil {
			return nil, err
		}
		if ok {
			if err := checkSums(path.Join(m.Dir, "go.sum"), gosum, o.Version, made); err != nil {
				return nil, err
			}
		}
	}

	// Every tag is checked before one is made: a tag that exists must
	// already name the commit the release gives it.
	byDir := append([]*repoModule(nil), mods...)
	sort.Slice(byDir, func(i, j int) bool { return byDir[i].Dir < byDir[j].Dir })
	type tag struct{ name, at string }
	var tags []string
	var missing []tag
	for _, m := range byDir {
		t := tag{m.Tag(o.Version), tagged(m, base, commit)}
		have, err := g.tagCommit(ctx, t.name)
		if err != nil {
			return nil, err
		}
		if have != "" && have != t.at {
			return nil, fmt.Errorf("tag %s exists at %s, but the release puts it at %s", t.name, have, t.at)
		}
		tags = append(tags, t.name)
		if have == "" {
			missing = append(missing, t)
		}
	}
	for _, t := range missing {
		if _, err := g.output(ctx, "tag", t.name, t.at); err != nil {
			return nil, err
		}
	}
	logf("release: %d tags (%d new), the file proxy is %s", len(tags), len(missing), proxy)
	return &outcome{Base: base, Commit: commit, Tags: tags}, nil
}

// rootOf returns the root module of mods, which discover guarantees.
func rootOf(mods []*repoModule) *repoModule {
	for _, m := range mods {
		if m.Dir == "" {
			return m
		}
	}
	panic("no root module")
}

// tagged is the commit m's tag names: the root module's is the commit
// released, whose root module the release commit does not change.
func tagged(m *repoModule, base, commit string) string {
	if m.Dir == "" {
		return base
	}
	return commit
}

// moduleFiles returns the zip and the go.mod of m at rev.
func moduleFiles(ctx context.Context, g repo, rev string, m *repoModule, mv module.Version) (zipData, gomod []byte, err error) {
	zipData, err = moduleZip(ctx, g, rev, m.Dir, mv)
	if err != nil {
		return nil, nil, err
	}
	gomod, ok, err := g.file(ctx, rev, path.Join(m.Dir, "go.mod"))
	if err == nil && !ok {
		err = fmt.Errorf("%s has no %s", rev, path.Join(m.Dir, "go.mod"))
	}
	return zipData, gomod, err
}

// checkSums fails when a line of the go.sum named name, for a module of the
// release at version, does not have the hash that module's tag serves.
func checkSums(name string, gosum []byte, version string, want map[string]sums) error {
	for n, line := range strings.Split(string(gosum), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		s, ok := want[f[0]]
		if !ok {
			continue
		}
		switch {
		case f[1] == version && f[2] != s.Zip:
			return fmt.Errorf("%s:%d: %s %s is %s, but its tag serves %s", name, n+1, f[0], f[1], f[2], s.Zip)
		case f[1] == version+"/go.mod" && f[2] != s.GoMod:
			return fmt.Errorf("%s:%d: %s %s is %s, but its tag serves %s", name, n+1, f[0], f[1], f[2], s.GoMod)
		}
	}
	return nil
}

// identity returns the environment that gives the release commit the
// author, committer and dates of the commit released, and that commit's
// time. Running release again for the same commit and version so makes the
// same commit, and the tags it pushed before still match.
func identity(ctx context.Context, g repo, base string) ([]string, time.Time, error) {
	out, err := g.output(ctx, "show", "-s", "--date=raw", "--format=%cn%n%ce%n%cd%n%ct", base)
	if err != nil {
		return nil, time.Time{}, err
	}
	f := strings.Split(out, "\n")
	if len(f) != 4 {
		return nil, time.Time{}, fmt.Errorf("git show %s: unexpected output %q", base, out)
	}
	sec, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return nil, time.Time{}, err
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + f[0], "GIT_AUTHOR_EMAIL=" + f[1], "GIT_AUTHOR_DATE=" + f[2],
		"GIT_COMMITTER_NAME=" + f[0], "GIT_COMMITTER_EMAIL=" + f[1], "GIT_COMMITTER_DATE=" + f[2],
	}
	return env, time.Unix(sec, 0), nil
}

func message(version string) string {
	return "chore(release): pin the Go modules to " + version + "\n\n" +
		"Every module requires its sibling modules at " + version + ", with no\n" +
		"replace directive, and its go.sum has their hashes, so the modules\n" +
		"build outside a clone. This commit is on no branch: the module tags\n" +
		"(<dir>/" + version + ") point at it, and " + version + " at its parent.\n"
}

// goEnv is the environment of go mod tidy: the file proxy first, then the
// local module cache, then fallback; a module cache of its own, so a
// version tidied before never comes back from a cache; and no go env file,
// whose GOPRIVATE or GONOPROXY could send these modules elsewhere.
func goEnv(proxy, modCache, fallback, rootPath string) ([]string, error) {
	first, err := fileURL(proxy)
	if err != nil {
		return nil, err
	}
	proxies := []string{first}
	if dl := downloadCache(); dl != "" {
		u, err := fileURL(dl)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, u)
	}
	if fallback != "" {
		proxies = append(proxies, fallback)
	}
	set := map[string]string{
		"GOENV":       "off",
		"GOWORK":      "off",
		"GOFLAGS":     "-modcacherw",
		"GOPROXY":     strings.Join(proxies, ","),
		"GONOSUMDB":   rootPath, // the release's modules are not in the checksum database yet
		"GOPRIVATE":   "",
		"GONOPROXY":   "",
		"GOINSECURE":  "",
		"GOMODCACHE":  modCache,
		"GOTOOLCHAIN": "local",
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := set[k]; !replaced {
			env = append(env, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+set[k])
	}
	return env, nil
}

// downloadCache is the download directory of the local module cache, which
// serves as a GOPROXY of every module this machine has downloaded, or "".
func downloadCache() string {
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return ""
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "cache", "download")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// goModTidy runs go mod tidy in dir.
func goModTidy(ctx context.Context, dir string, env []string) error {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, "go", "mod", "tidy")
	c.Dir, c.Env, c.Stdout, c.Stderr = dir, env, &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("go mod tidy in %s: %w\n%s", dir, err, out.String())
	}
	return nil
}
