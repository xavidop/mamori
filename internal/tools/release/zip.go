package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// moduleZip builds the zip of module mv from rev (a commit or a tree), with
// the module in directory dir of the repository ("" for the root module).
//
// It reproduces what the go command, and so proxy.golang.org, builds from a
// tag (cmd/go/internal/modfetch, codeRepo.Zip): every file git archive
// writes for dir, plus the repository's LICENSE when dir has none, through
// golang.org/x/mod/zip, which drops the files of nested modules, vendor
// directories and symlinks. The go command runs git archive with
// export-ignore and export-subst turned off and with core.autocrlf=input, so
// without the attributes checkAttributes refuses, git archive writes every
// blob as committed, and this reads the blobs directly.
//
// It is not modzip.CreateFromDir on a checkout: that would add the .git file
// of a worktree to the root module and leave the LICENSE out of the others.
func moduleZip(ctx context.Context, g repo, rev, dir string, mv module.Version) ([]byte, error) {
	entries, err := g.tree(ctx, rev, dir)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	type want struct {
		name string
		e    entry
	}
	var wants []want
	haveLICENSE := false
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.path, prefix)
		if !ok || name == "" || e.kind != "blob" {
			continue // a git submodule (kind commit) is an empty directory in git archive
		}
		wants = append(wants, want{name, e})
		if name == "LICENSE" {
			haveLICENSE = true
		}
	}
	if !haveLICENSE && dir != "" {
		// The go command reads it with git cat-file and adds it as a
		// regular file, whatever its mode.
		root, err := g.tree(ctx, rev, "LICENSE")
		if err != nil {
			return nil, err
		}
		for _, e := range root {
			if e.path == "LICENSE" && e.kind == "blob" {
				e.mode = "100644"
				wants = append(wants, want{"LICENSE", e})
			}
		}
	}
	oids := make([]string, len(wants))
	for i, w := range wants {
		oids[i] = w.e.oid
	}
	data, err := g.blobs(ctx, oids)
	if err != nil {
		return nil, err
	}
	files := make([]modzip.File, len(wants))
	for i, w := range wants {
		files[i] = blobFile{name: w.name, mode: fileMode(w.e.mode), data: data[w.e.oid]}
	}
	var buf bytes.Buffer
	if err := modzip.Create(&buf, mv, files); err != nil {
		return nil, fmt.Errorf("zip of %s: %w", mv, err)
	}
	return buf.Bytes(), nil
}

// fileMode maps a git tree mode onto the mode git archive gives the file.
func fileMode(gitMode string) fs.FileMode {
	switch gitMode {
	case "120000":
		return fs.ModeSymlink | 0o777
	case "100755":
		return 0o755
	default:
		return 0o644
	}
}

// blobFile is one file of a module zip, read from git.
type blobFile struct {
	name string
	mode fs.FileMode
	data []byte
}

func (f blobFile) Path() string                 { return f.name }
func (f blobFile) Lstat() (fs.FileInfo, error)  { return blobInfo{f}, nil }
func (f blobFile) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.data)), nil }

type blobInfo struct{ f blobFile }

func (i blobInfo) Name() string       { return path.Base(i.f.name) }
func (i blobInfo) Size() int64        { return int64(len(i.f.data)) }
func (i blobInfo) Mode() fs.FileMode  { return i.f.mode }
func (i blobInfo) ModTime() time.Time { return time.Time{} }
func (i blobInfo) IsDir() bool        { return false }
func (i blobInfo) Sys() any           { return nil }

// convertingAttrs are the git attributes that make git archive write a file
// differently from its blob: line endings, $Id$ expansion, filters and
// encodings. (export-ignore and export-subst are not here: the go command
// turns them off.)
var convertingAttrs = map[string]bool{
	"text": true, "eol": true, "crlf": true, "ident": true,
	"filter": true, "working-tree-encoding": true,
}

// checkAttributes fails when a .gitattributes file of rev sets an attribute
// that makes git archive write a file differently from its blob. moduleZip
// reads blobs, so its zips would no longer match the go command's.
func checkAttributes(ctx context.Context, g repo, rev string) error {
	entries, err := g.tree(ctx, rev, "")
	if err != nil {
		return err
	}
	var found []entry
	for _, e := range entries {
		if path.Base(e.path) == ".gitattributes" && e.kind == "blob" {
			found = append(found, e)
		}
	}
	if len(found) == 0 {
		return nil
	}
	oids := make([]string, len(found))
	for i, e := range found {
		oids[i] = e.oid
	}
	data, err := g.blobs(ctx, oids)
	if err != nil {
		return err
	}
	for _, e := range found {
		sc := bufio.NewScanner(bytes.NewReader(data[e.oid]))
		for n := 1; sc.Scan(); n++ {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			for _, attr := range fields[1:] {
				if strings.HasPrefix(attr, "-") || strings.HasPrefix(attr, "!") {
					continue // unset: no conversion
				}
				name, _, _ := strings.Cut(attr, "=")
				if convertingAttrs[name] {
					return fmt.Errorf("%s:%d sets %s, which changes what git archive writes for a module zip; the release tool reads blobs as committed and does not support it", e.path, n, attr)
				}
			}
		}
	}
	return nil
}

// sums are the go.sum hashes of one module version.
type sums struct {
	Zip, GoMod string // h1: hashes, of the zip and of the go.mod
}

// hashes computes the go.sum hashes of a module zip and its go.mod, the way
// the go command does (dirhash.HashZip over the zip's file names and
// contents; dirhash.Hash1 over the go.mod alone).
func hashes(zipData, gomod []byte) (sums, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return sums{}, err
	}
	byName := make(map[string]*zip.File, len(zr.File))
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		byName[f.Name] = f
		names = append(names, f.Name)
	}
	z, err := dirhash.Hash1(names, func(name string) (io.ReadCloser, error) { return byName[name].Open() })
	if err != nil {
		return sums{}, err
	}
	m, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(gomod)), nil
	})
	if err != nil {
		return sums{}, err
	}
	return sums{Zip: z, GoMod: m}, nil
}
