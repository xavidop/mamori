package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

// explainUsage renders the built-in scheme list from the set itself, so the
// help text cannot drift from what the SENSITIVE column actually reports.
var explainUsage = `usage: mamori explain [patterns...] [--type=Name] [--json] [--secret-schemes=list]

Explain reads Go source (via golang.org/x/tools/go/packages) and prints
every struct type with at least one source: tagged field: its field paths,
Go types, source chains, defaults, and which fields are sensitive. It never
resolves anything (no network calls, no secret managers contacted).

  patterns          Go package patterns to load (default: the current
                    directory, same as omitting a pattern to "go build").
  --type            only explain the struct type with this name
  --json            emit the result as JSON instead of a text table
  --secret-schemes  comma-separated extra schemes to count as secret-bearing
                    in the SENSITIVE column, added to the built-in set. Use
                    this for a custom provider, e.g.
                    --secret-schemes=mysecrets,corp-kv

A field is SENSITIVE when its type is secret.String / secret.Bytes, or when
any ref in its chain uses a secret-bearing scheme. Built-in secret-bearing
schemes:
  ` + strings.Join(sourcetag.DefaultSecretSchemes().Sorted(), ", ") + `
`

// explainCmd is the mamori explain subcommand. It writes its output to
// stdout and any errors to stderr (both injected so tests never touch the
// real os.Stdout/os.Stderr), and returns the process exit code: 0 on
// success, 1 on a usage or package-load error (the 0-4 live exit codes are
// reserved for doctor/status, which classify a running process's health,
// not a static-analysis failure).
func explainCmd(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, explainUsage)
	}
	patterns, typeName, jsonOut, schemes, err := parseExplainArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, explainUsage)
		return 1
	}

	structs, err := Extract(patterns, typeName, schemes)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori explain: %v\n", err)
		return 1
	}

	// Drop validate-only fields once, here, so the table and the JSON agree.
	// explain lists what mamori reads, and a field the application populates
	// itself has no ref to list. Filtering only the table would leak it into
	// --json, which is what "mamori diff" consumes.
	structs = withoutValidateOnly(structs)

	if jsonOut {
		return writeExplainJSON(stdout, stderr, structs)
	}
	writeExplainTable(stdout, structs)
	return 0
}

// withoutValidateOnly returns structs with every KindValidate field removed.
// It copies rather than filtering in place: Extract's result is shared with no
// one today, but explain is the only command that wants this narrowing, and a
// mutating filter here would be a trap for the next caller added.
func withoutValidateOnly(structs []StructInfo) []StructInfo {
	out := make([]StructInfo, 0, len(structs))
	for _, s := range structs {
		kept := make([]Field, 0, len(s.Fields))
		for _, f := range s.Fields {
			if f.Kind == KindValidate {
				continue
			}
			kept = append(kept, f)
		}
		s.Fields = kept
		out = append(out, s)
	}
	return out
}

// parseExplainArgs splits args into package patterns and the --type/--json
// flags. It scans by recognized flag shape rather than using flag.FlagSet,
// so patterns and flags may appear in either order (e.g. both
// "explain ./... --json" and "explain --json ./..." work), matching how
// this command is documented.
// The returned schemes is nil unless --secret-schemes was given, so the
// common case keeps using the shared built-in set (see Extract).
func parseExplainArgs(args []string) (patterns []string, typeName string, jsonOut bool, schemes sourcetag.SchemeSet, err error) {
	var extra string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if value, consumed, matchErr := matchSecretSchemes("explain", args, i); matchErr != nil {
			return nil, "", false, nil, matchErr
		} else if consumed > 0 {
			extra = value
			i += consumed - 1
			continue
		}
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "--type" || a == "-type":
			i++
			if i >= len(args) {
				return nil, "", false, nil, fmt.Errorf("mamori explain: %s requires a value", a)
			}
			typeName = args[i]
		case strings.HasPrefix(a, "--type="):
			typeName = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "-type="):
			typeName = strings.TrimPrefix(a, "-type=")
		case strings.HasPrefix(a, "-"):
			return nil, "", false, nil, fmt.Errorf("mamori explain: unknown flag %q", a)
		default:
			patterns = append(patterns, a)
		}
	}

	schemes, err = secretSchemeSet("explain", extra)
	if err != nil {
		return nil, "", false, nil, err
	}
	return patterns, typeName, jsonOut, schemes, nil
}

// writeExplainJSON encodes structs as indented JSON. It returns 1 (and
// writes to stderr) only on an encoding failure, which should not happen in
// practice since Field/StructInfo are plain marshalable data.
func writeExplainJSON(stdout, stderr io.Writer, structs []StructInfo) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if structs == nil {
		structs = []StructInfo{}
	}
	if err := enc.Encode(structs); err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori explain: encoding JSON: %v\n", err)
		return 1
	}
	return 0
}

// writeExplainTable prints one aligned table per struct (FIELD, TYPE,
// CHAIN, DEFAULT, OPTIONAL, SENSITIVE), separated by a "package.TypeName"
// banner line so multiple structs stay unambiguous in one report.
func writeExplainTable(stdout io.Writer, structs []StructInfo) {
	for i, s := range structs {
		if i > 0 {
			_, _ = fmt.Fprintln(stdout)
		}
		_, _ = fmt.Fprintf(stdout, "%s.%s\n", s.Package, s.TypeName)

		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "FIELD\tTYPE\tCHAIN\tDEFAULT\tOPTIONAL\tSENSITIVE")
		for _, f := range s.Fields {
			def := "-"
			if f.HasDefault {
				def = f.Default
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				f.Path, f.GoType, strings.Join(f.Refs, ", "), def,
				strconv.FormatBool(f.Optional), strconv.FormatBool(f.Sensitive))
		}
		_ = tw.Flush()
	}
}
