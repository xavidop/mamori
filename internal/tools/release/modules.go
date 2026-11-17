package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// repoModule is one Go module of the repository.
type repoModule struct {
	Dir   string        // slash-separated, from the repository root; "" for the root module
	Path  string        // the module path
	Deps  []*repoModule // the sibling modules its go.mod requires
	GoMod []byte        // its go.mod at the commit released
}

// Tag is the module's tag for version: vX.Y.Z for the root module,
// <dir>/vX.Y.Z for the others, which is where the go command looks.
func (m *repoModule) Tag(version string) string {
	if m.Dir == "" {
		return version
	}
	return m.Dir + "/" + version
}

// String names the module for messages.
func (m *repoModule) String() string {
	if m.Dir == "" {
		return "the root module"
	}
	return m.Dir
}

// released reports whether a go.mod at p belongs to a module of the release:
// not under testdata (fixtures), internal (the repository's own tooling),
// vendor, or a directory the go command ignores (. or _).
func released(p string) bool {
	for _, elem := range strings.Split(path.Dir(p), "/") {
		if elem == "testdata" || elem == "internal" || elem == "vendor" ||
			(elem != "." && (strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_"))) {
			return false
		}
	}
	return true
}

// discover finds the modules of rev and returns them in dependency order: a
// module comes after every sibling it requires.
func discover(ctx context.Context, g repo, rev string) ([]*repoModule, error) {
	entries, err := g.tree(ctx, rev, "")
	if err != nil {
		return nil, err
	}
	var gomods []entry
	for _, e := range entries {
		if path.Base(e.path) == "go.mod" && e.kind == "blob" && released(e.path) {
			gomods = append(gomods, e)
		}
	}
	oids := make([]string, len(gomods))
	for i, e := range gomods {
		oids[i] = e.oid
	}
	data, err := g.blobs(ctx, oids)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*repoModule{}
	var root *repoModule
	var mods []*repoModule
	for _, e := range gomods {
		dir := path.Dir(e.path)
		if dir == "." {
			dir = ""
		}
		f, err := modfile.ParseLax(e.path, data[e.oid], nil)
		if err != nil {
			return nil, err
		}
		if f.Module == nil {
			return nil, fmt.Errorf("%s has no module line", e.path)
		}
		m := &repoModule{Dir: dir, Path: f.Module.Mod.Path, GoMod: data[e.oid]}
		if other := byPath[m.Path]; other != nil {
			return nil, fmt.Errorf("%s and %s are both module %s", other, m, m.Path)
		}
		byPath[m.Path] = m
		mods = append(mods, m)
		if dir == "" {
			root = m
		}
	}
	if root == nil {
		return nil, fmt.Errorf("%s has no go.mod at the repository root", rev)
	}
	for _, m := range mods {
		// The go command finds module <root>/<dir> in directory <dir> of
		// the repository, at the tag <dir>/vX.Y.Z.
		if m != root && m.Path != root.Path+"/"+m.Dir {
			return nil, fmt.Errorf("%s/go.mod is module %s: in that directory it must be %s/%s", m.Dir, m.Path, root.Path, m.Dir)
		}
		f, err := modfile.ParseLax(m.Dir+"/go.mod", m.GoMod, nil)
		if err != nil {
			return nil, err
		}
		seen := map[*repoModule]bool{}
		for _, r := range f.Require {
			if dep := byPath[r.Mod.Path]; dep != nil && !seen[dep] {
				seen[dep] = true
				m.Deps = append(m.Deps, dep)
			}
		}
	}
	return order(mods)
}

// order sorts mods so that every module comes after the siblings it
// requires, and by directory otherwise.
func order(mods []*repoModule) ([]*repoModule, error) {
	sort.Slice(mods, func(i, j int) bool { return mods[i].Dir < mods[j].Dir })
	done := map[*repoModule]bool{}
	var sorted []*repoModule
	for len(sorted) < len(mods) {
		progress := false
		for _, m := range mods {
			if done[m] {
				continue
			}
			ready := true
			for _, d := range m.Deps {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				done[m] = true
				sorted = append(sorted, m)
				progress = true
			}
		}
		if !progress {
			var cycle []string
			for _, m := range mods {
				if !done[m] {
					cycle = append(cycle, m.String())
				}
			}
			return nil, fmt.Errorf("the modules %s require each other in a cycle", strings.Join(cycle, ", "))
		}
	}
	return sorted, nil
}

// siblings is the set of module paths of the release.
type siblings map[string]bool

func siblingsOf(mods []*repoModule) siblings {
	s := siblings{}
	for _, m := range mods {
		s[m.Path] = true
	}
	return s
}

// pin returns m's go.mod for the release, or nil when it has nothing to
// change: every sibling it requires at version, and none of the replace
// directives that point a sibling at its directory. Any other replace, and
// any exclude, is an error: go install refuses a module that has one, and a
// library's would not reach the modules that require it.
func pin(m *repoModule, sib siblings, version string) ([]byte, error) {
	name := path.Join(m.Dir, "go.mod")
	f, err := modfile.Parse(name, m.GoMod, nil)
	if err != nil {
		return nil, err
	}
	// DropReplace clears the entries it drops, so check them all first.
	var drop []module.Version
	for _, r := range f.Replace {
		if !sib[r.Old.Path] || !modfile.IsDirectoryPath(r.New.Path) {
			return nil, fmt.Errorf("%s: replace %s => %s: only a replace that points a sibling module at its directory can be dropped for the release; remove this one on the branch",
				name, strings.TrimSpace(r.Old.Path+" "+r.Old.Version), strings.TrimSpace(r.New.Path+" "+r.New.Version))
		}
		drop = append(drop, r.Old)
	}
	for _, old := range drop {
		if err := f.DropReplace(old.Path, old.Version); err != nil {
			return nil, err
		}
	}
	for _, x := range f.Exclude {
		return nil, fmt.Errorf("%s: exclude %s %s: go install refuses a module with an exclude; remove it on the branch", name, x.Mod.Path, x.Mod.Version)
	}
	var deps []string
	for _, r := range f.Require {
		if sib[r.Mod.Path] && r.Mod.Version != version {
			deps = append(deps, r.Mod.Path)
		}
	}
	if len(drop) == 0 && len(deps) == 0 {
		return nil, nil
	}
	for _, p := range deps {
		if err := f.AddRequire(p, version); err != nil {
			return nil, err
		}
	}
	f.Cleanup()
	return f.Format()
}

// checkPinned fails unless the go.mod data (after go mod tidy) requires every
// sibling at version and has no replace or exclude.
func checkPinned(name string, data []byte, sib siblings, version string) error {
	f, err := modfile.Parse(name, data, nil)
	if err != nil {
		return err
	}
	if len(f.Replace) > 0 || len(f.Exclude) > 0 {
		return fmt.Errorf("%s still has a replace or an exclude", name)
	}
	for _, r := range f.Require {
		if sib[r.Mod.Path] && r.Mod.Version != version {
			return fmt.Errorf("%s requires %s, not %s", name, r.Mod, version)
		}
	}
	return nil
}
