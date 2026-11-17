package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// addToProxy writes mv to the file proxy (GOPROXY=file://dir): its .info,
// .mod and .zip, and its version in @v/list.
func addToProxy(dir string, mv module.Version, t time.Time, gomod, zipData []byte) error {
	escPath, err := module.EscapePath(mv.Path)
	if err != nil {
		return err
	}
	escVersion, err := module.EscapeVersion(mv.Version)
	if err != nil {
		return err
	}
	vdir := filepath.Join(dir, filepath.FromSlash(escPath), "@v")
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return err
	}
	info, err := json.Marshal(struct {
		Version string
		Time    time.Time
	}{mv.Version, t.UTC()})
	if err != nil {
		return err
	}
	for ext, data := range map[string][]byte{".info": info, ".mod": gomod, ".zip": zipData} {
		if err := os.WriteFile(filepath.Join(vdir, escVersion+ext), data, 0o644); err != nil {
			return err
		}
	}
	list := filepath.Join(vdir, "list")
	old, err := os.ReadFile(list)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	versions := []string{mv.Version}
	for _, v := range strings.Fields(string(old)) {
		if v != mv.Version {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return semver.Compare(versions[i], versions[j]) < 0 })
	return os.WriteFile(list, []byte(strings.Join(versions, "\n")+"\n"), 0o644)
}

// fileURL is a GOPROXY entry for a local directory.
func fileURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}
