// This file implements the `mamori vet` subcommand and the dual-mode seam
// that lets the same binary act as a `go vet` tool. Both run the vetcheck
// analyzer (internal/vetcheck), which flags struct fields that pull one of
// the built-in secret-bearing schemes (sourcetag.DefaultSecretSchemes) into
// a plain string or []byte instead of the redacting secret.String /
// secret.Bytes types.
//
// vetCmd is the standalone driver: it loads packages with
// golang.org/x/tools/go/packages and runs the analyzer in-process via
// golang.org/x/tools/go/analysis/checker (which resolves the analyzer's
// inspect.Analyzer requirement itself). The go-vet-tool half lives in
// main.go, which hands off to unitchecker.Main when isVetToolInvocation
// (below) recognizes the go vet driver's invocation.
package main

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
	"github.com/xavidop/mamori/cmd/mamori/internal/vetcheck"
)

// vetUsage renders the built-in scheme list from the set itself, so the help
// text cannot drift from what the analyzer actually flags.
var vetUsage = `usage: mamori vet [flags] [patterns...]

Vet reports any struct field that binds a secret-bearing source to a plain
string or []byte instead of the redacting secret.String / secret.Bytes types.

Built-in secret-bearing schemes:
  ` + strings.Join(sourcetag.DefaultSecretSchemes().Sorted(), ", ") + `

  patterns          Go package patterns to load (default: ./..., matching how
                    a linter is usually run over a whole module).

  --secret-schemes  Comma-separated extra schemes to treat as secret-bearing,
                    added to the built-in set. Use this for a custom provider
                    whose scheme mamori does not ship, e.g.
                    --secret-schemes=mysecrets,corp-kv

Diagnostics are written to stderr in go vet style (pos: message). Exit code is
1 if any diagnostic is reported or a package fails to load, 0 when clean.

The same binary also works as a go vet tool, where the flag is namespaced by
the analyzer:

  go vet -vettool=$(which mamori) ./...
  go vet -vettool=$(which mamori) -mamorivet.secret-schemes=mysecrets ./...
`

// vetCmd is the mamori vet subcommand. It loads the packages matching
// patterns (defaulting to ./...) and runs the vetcheck analyzer over them,
// writing diagnostics to stderr and returning the process exit code: 1 if any
// diagnostic is reported or a package fails to load, 0 when clean. stdout and
// stderr are injected so tests never touch the real os.Stdout/os.Stderr.
func vetCmd(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, vetUsage)
	}
	_ = stdout // vet writes diagnostics to stderr, matching go vet; stdout is unused.

	patterns, schemes, err := parseVetArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, vetUsage)
		return 1
	}
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	// Drive the analyzer through its own FlagSet, the same one unitchecker
	// populates in go-vet-tool mode, so both modes configure the scheme set
	// through a single declaration (see vetcheck's -secret-schemes flag).
	if err := vetcheck.Analyzer.Flags.Set("secret-schemes", schemes); err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori vet: --secret-schemes: %v\n", err)
		return 1
	}

	// The analyzer needs full type information, and checker.Analyze panics on
	// a package loaded without TypesSizes, so NeedTypesSizes is required in
	// addition to the extract.go set. NeedDeps|NeedImports let the driver
	// satisfy the analyzer's inspect.Analyzer requirement across the graph.
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedTypesSizes |
			packages.NeedDeps | packages.NeedImports,
		// Analyze test files too. The go vet driver does, so without this the
		// two modes would disagree about the same packages: a config struct
		// declared in a _test.go file would be flagged by
		// `go vet -vettool=mamori` but not by `mamori vet`.
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori vet: loading packages %v: %v\n", patterns, err)
		return 1
	}

	// Report load/type errors to the injected stderr (packages.PrintErrors
	// writes to the real os.Stderr, which tests can't capture) and bail before
	// analysis so a broken package is a hard failure, not a silent clean pass.
	nErrs := 0
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			_, _ = fmt.Fprintln(stderr, e)
			nErrs++
		}
		if pkg.Module != nil && pkg.Module.Error != nil {
			_, _ = fmt.Fprintln(stderr, pkg.Module.Error.Err)
			nErrs++
		}
	})
	if nErrs > 0 {
		_, _ = fmt.Fprintf(stderr, "mamori vet: %d error(s) loading packages %v\n", nErrs, patterns)
		return 1
	}

	graph, err := checker.Analyze([]*analysis.Analyzer{vetcheck.Analyzer}, pkgs, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori vet: %v\n", err)
		return 1
	}

	// PrintText emits both per-action errors and root diagnostics in go vet
	// style (contextLines=0 prints just "pos: message", no source snippet).
	if err := graph.PrintText(stderr, 0); err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori vet: writing diagnostics: %v\n", err)
		return 1
	}

	// Set the exit code from what was reported: any analysis error, or any
	// diagnostic on a root action (the vetcheck-per-package nodes), means a
	// non-clean run. The exact count does not matter, only whether it is >0.
	reported := 0
	for act := range graph.All() {
		if act.Err != nil {
			reported++
			continue
		}
		if act.IsRoot {
			reported += len(act.Diagnostics)
		}
	}
	if reported > 0 {
		return 1
	}
	return 0
}

// parseVetArgs splits args into package patterns and the --secret-schemes
// value, accepting the same flag forms as the other static commands (both
// "--flag value" and "--flag=value", with one or two leading dashes). Any
// other flag is an error (top-level -h/--help is handled by run() before it
// ever reaches here).
//
// The scheme list is validated here rather than only inside the analyzer so a
// typo such as --secret-schemes=mysecrets://prod fails immediately with usage,
// instead of surfacing later as an analysis error.
func parseVetArgs(args []string) (patterns []string, schemes string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if value, consumed, matchErr := matchSecretSchemes("vet", args, i); matchErr != nil {
			return nil, "", matchErr
		} else if consumed > 0 {
			schemes = value
			i += consumed - 1
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, "", fmt.Errorf("mamori vet: unknown flag %q", a)
		}
		patterns = append(patterns, a)
	}
	if _, err := sourcetag.ParseSchemeList(schemes); err != nil {
		return nil, "", fmt.Errorf("mamori vet: --secret-schemes: %w", err)
	}
	return patterns, schemes, nil
}

// isVetToolInvocation reports whether os.Args indicate that the go vet driver
// is running this binary as its -vettool, in which case main() must hand off
// to unitchecker.Main rather than the CLI dispatcher.
//
// The go vet driver invokes the tool in exactly three shapes (confirmed by
// tracing a real `go vet -vettool=mamori` run):
//
//	mamori -flags                # discover the tool's flags (JSON)
//	mamori -V=full               # version probe
//	mamori -json <unit>.cfg      # analyze one unit described by a config file
//
// So the tell is either a discovery/version probe (-flags, -V, -V=full) or a
// trailing argument ending in ".cfg" (the unit config file, which go vet
// passes last, after -json). Neither collides with normal CLI usage: no mamori
// subcommand takes a .cfg path, and `mamori vet ./...` ends in a package
// pattern, so it flows through run().
func isVetToolInvocation(args []string) bool {
	for _, a := range args {
		if a == "-flags" || a == "-V" || strings.HasPrefix(a, "-V=") {
			return true
		}
	}
	if len(args) > 0 && strings.HasSuffix(args[len(args)-1], ".cfg") {
		return true
	}
	return false
}
