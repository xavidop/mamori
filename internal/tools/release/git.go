package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// repo runs git in one directory of a repository.
type repo struct {
	dir string
	env []string // added to the process environment
}

func (g repo) command(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = g.dir
	c.Env = append(os.Environ(), g.env...)
	return c
}

// raw runs git and returns its standard output as it is.
func (g repo) raw(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	c := g.command(ctx, args...)
	c.Stdin, c.Stdout, c.Stderr = stdin, &stdout, &stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// output runs git and returns its standard output, trimmed.
func (g repo) output(ctx context.Context, args ...string) (string, error) {
	out, err := g.raw(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// commitOf resolves rev to a commit ID.
func (g repo) commitOf(ctx context.Context, rev string) (string, error) {
	return g.output(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
}

// tagCommit returns the commit tag points at, or "" when there is no such tag.
func (g repo) tagCommit(ctx context.Context, tag string) (string, error) {
	out, err := g.raw(ctx, nil, "tag", "--list", tag)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", err
	}
	return g.commitOf(ctx, "refs/tags/"+tag)
}

// entry is one line of git ls-tree.
type entry struct {
	mode, kind, oid, path string
}

// tree lists the files of rev under dir ("" for all of them), recursively,
// with paths from the repository root.
func (g repo) tree(ctx context.Context, rev, dir string) ([]entry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--end-of-options", rev}
	if dir != "" {
		args = append(args, dir)
	}
	out, err := g.raw(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []entry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected line %q", rec)
		}
		entries = append(entries, entry{mode: f[0], kind: f[1], oid: f[2], path: p})
	}
	return entries, nil
}

// blobs reads the content of every object in oids with one git cat-file.
func (g repo) blobs(ctx context.Context, oids []string) (map[string][]byte, error) {
	data := make(map[string][]byte, len(oids))
	if len(oids) == 0 {
		return data, nil
	}
	in := strings.Join(oids, "\n") + "\n"
	out, err := g.raw(ctx, strings.NewReader(in), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	for range oids {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: %w", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("git cat-file: %s", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file: %s: %w", strings.TrimSpace(header), err)
		}
		b := make([]byte, size+1) // the content and its trailing newline
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("git cat-file %s: %w", f[0], err)
		}
		data[f[0]] = b[:size]
	}
	return data, nil
}

// file reads one file of rev; ok is false when rev has no blob at p.
func (g repo) file(ctx context.Context, rev, p string) (data []byte, ok bool, err error) {
	entries, err := g.tree(ctx, rev, p)
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		if e.path == p && e.kind == "blob" {
			b, err := g.blobs(ctx, []string{e.oid})
			return b[e.oid], err == nil, err
		}
	}
	return nil, false, nil
}
