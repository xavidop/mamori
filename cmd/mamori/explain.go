package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

const explainUsage = `usage: mamori explain [patterns...] [--type=Name] [--json]

Explain reads Go source (via golang.org/x/tools/go/packages) and prints
every struct type with at least one source: tagged field: its field paths,
Go types, source chains, defaults, and which fields are sensitive. It never
resolves anything (no network calls, no secret managers contacted).

  patterns   Go package patterns to load (default: the current directory,
             same as omitting a pattern to "go build"). Example: ./...
  --type     only explain the struct type with this name
  --json     emit the result as JSON instead of a text table
`

// explainCmd is the mamori explain subcommand. It writes its output to
// stdout and any errors to stderr (both injected so tests never touch the
// real os.Stdout/os.Stderr), and returns the process exit code: 0 on
// success, 1 on a usage or package-load error (the 0-4 live exit codes are
// reserved for doctor/status, which classify a running process's health,
// not a static-analysis failure).
func explainCmd(args []string, stdout, stderr io.Writer) int {
	patterns, typeName, jsonOut, err := parseExplainArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprint(stderr, explainUsage)
		return 1
	}

	structs, err := Extract(patterns, typeName)
	if err != nil {
		fmt.Fprintf(stderr, "mamori explain: %v\n", err)
		return 1
	}

	if jsonOut {
		return writeExplainJSON(stdout, stderr, structs)
	}
	writeExplainTable(stdout, structs)
	return 0
}

// parseExplainArgs splits args into package patterns and the --type/--json
// flags. It scans by recognized flag shape rather than using flag.FlagSet,
// so patterns and flags may appear in either order (e.g. both
// "explain ./... --json" and "explain --json ./..." work), matching how
// this command is documented.
func parseExplainArgs(args []string) (patterns []string, typeName string, jsonOut bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "--type" || a == "-type":
			i++
			if i >= len(args) {
				return nil, "", false, fmt.Errorf("mamori explain: %s requires a value", a)
			}
			typeName = args[i]
		case strings.HasPrefix(a, "--type="):
			typeName = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "-type="):
			typeName = strings.TrimPrefix(a, "-type=")
		case strings.HasPrefix(a, "-"):
			return nil, "", false, fmt.Errorf("mamori explain: unknown flag %q", a)
		default:
			patterns = append(patterns, a)
		}
	}
	return patterns, typeName, jsonOut, nil
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
		fmt.Fprintf(stderr, "mamori explain: encoding JSON: %v\n", err)
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
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s.%s\n", s.Package, s.TypeName)

		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "FIELD\tTYPE\tCHAIN\tDEFAULT\tOPTIONAL\tSENSITIVE")
		for _, f := range s.Fields {
			def := "-"
			if f.HasDefault {
				def = f.Default
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				f.Path, f.GoType, strings.Join(f.Refs, ", "), def,
				strconv.FormatBool(f.Optional), strconv.FormatBool(f.Sensitive))
		}
		tw.Flush()
	}
}
