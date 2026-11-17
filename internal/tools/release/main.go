// Command release makes every Go module of this repository consumable at a
// release version, from outside a clone.
//
// On a branch, every module requires its sibling modules at v0.0.0 and points
// at their directories with replace directives. A module outside a clone
// cannot use those, and go install refuses a module that has any replace.
// Given a version vX.Y.Z and the commit semantic-release tagged with it,
// release writes a release commit on top of that commit, on no branch, in
// which every module requires its siblings at vX.Y.Z with no replace, and
// each go.sum has their hashes. go mod tidy completes each go.sum, module by
// module in dependency order, against a file:// GOPROXY that release fills
// with the sibling modules' .info, .mod and .zip. The zips are built with
// golang.org/x/mod/zip from the git tree, the way the go command builds them
// from a tag, so proxy.golang.org serves the same hashes.
//
// Then it tags the modules. The root module's tag, vX.Y.Z, stays on (or is
// created at) the commit given: the release commit changes no file of the
// root module, so both commits hold the same root module. Every other module
// gets <dir>/vX.Y.Z on the release commit. Modules under an internal or
// testdata directory (this tool's own module, internal/tools) are not
// released.
//
// release prints the tags, one per line, and pushes nothing. Run it from the
// repository root:
//
//	go -C internal/tools run ./release -version vX.Y.Z [-commit REV] [-proxy DIR]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// errUsage marks a usage error (exit code 2).
var errUsage = errors.New("usage")

// run is main without the process: it returns the exit code, 0 for a
// release, 1 when it failed, 2 for a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "release:", err)
		return 2
	}
	out, err := cut(ctx, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "release:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	// The tags are the output a workflow pushes: losing one is a failure.
	if _, err := fmt.Fprintln(stdout, strings.Join(out.Tags, "\n")); err != nil {
		_, _ = fmt.Fprintln(stderr, "release:", err)
		return 1
	}
	return 0
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.Version, "version", "", "the version to release, vX.Y.Z (required)")
	fs.StringVar(&o.Commit, "commit", "", "the commit to release (default: the commit tag vX.Y.Z names, or HEAD)")
	fs.StringVar(&o.Repo, "repo", ".", "a directory of the repository")
	fs.StringVar(&o.Proxy, "proxy", "", "write the file proxy of the release here and keep it (default: a temporary directory)")
	fs.StringVar(&o.GOPROXY, "goproxy", "https://proxy.golang.org", "where third-party modules come from, after the local module cache")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: release -version vX.Y.Z [-commit REV] [-repo DIR] [-proxy DIR] [-goproxy URL]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.Version == "" {
		return o, errors.New("-version is required")
	}
	o.Log = stderr
	return o, nil
}
