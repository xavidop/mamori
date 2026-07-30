// Command mamori is the mamori CLI. It has two halves that never mix:
// static commands (explain, schema, policy, vet, diff) never resolve
// anything, and live commands (doctor, status) are thin clients of a
// running process's admin endpoint. Most static commands extract source:
// refs from Go source; diff is the exception, comparing two prior
// `explain --json` outputs, so it needs no source and no build.
package main

import (
	"fmt"
	"os"

	"golang.org/x/tools/go/analysis/unitchecker"

	"github.com/xavidop/mamori/cmd/mamori/internal/vetcheck"
)

// version is stamped by ldflags at release build time (see .goreleaser.yaml).
// "dev" is what a plain `go build` produces.
//
// .goreleaser.yaml's mamori build also passes -X main.commit=... and
// -X main.date=..., but neither commit nor date has a matching package-level
// var here, so both -X flags are silently no-ops (confirmed: `go build
// -ldflags "-X main.commit=..."` against a package with no such var succeeds
// with no warning). They were deliberately not added as unused vars:
// cmd/mamori is linted by golangci-lint's "unused" check (.golangci.yml,
// standard linter set), which flags an unexported package-level var with no
// reader, so `var commit, date string` with nothing printing them would fail
// CI lint. Wire them up together with whatever future command first has a
// real use for them (e.g. printing build provenance).
var version = "dev"

func main() {
	// Dual-mode seam: this one binary is both the mamori CLI and a `go vet`
	// tool. When the go vet driver runs us (via `go vet -vettool=mamori`), it
	// speaks the unitchecker protocol, not the CLI's subcommand grammar, so we
	// detect that invocation and hand off to unitchecker.Main before the normal
	// CLI dispatch ever sees the args. Everything else (including the standalone
	// `mamori vet ./...` subcommand) flows through run(). See isVetToolInvocation
	// in vet.go for exactly which arg shapes the go vet driver uses.
	if isVetToolInvocation(os.Args[1:]) {
		unitchecker.Main(vetcheck.Analyzer)
		return
	}
	os.Exit(run(os.Args[1:]))
}

const usage = `mamori is a CLI for the mamori configuration library.

Usage:
  mamori <command> [arguments]

Static commands (read source, never resolve):
  explain    Explain the config structs and source: refs found in a package
  schema     Emit a JSON Schema for a config struct
  policy     Emit a least-privilege access policy derived from source: refs
  diff       Diff two "mamori explain --json" outputs, with a privilege delta
  vet        Report config fields that pull a secret into a plain string/[]byte

Live commands (thin clients of a running process's admin endpoint):
  doctor     Diagnose a running process's config health
  status     Print a running process's config status

Other:
  help       Show this help
  version    Print the CLI version
`

// run is the testable core of main: it dispatches args[0] to a subcommand
// and returns the process exit code, rather than calling os.Exit itself.
//
// Exit code 2 is reserved for usage errors (missing or unknown subcommand),
// distinct from the live commands' own exit codes (doctor/status use other
// codes to distinguish unreachable, admin-off, and auth-failed outcomes).
func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}

	switch args[0] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(os.Stdout, usage)
		return 0
	case "version", "--version":
		_, _ = fmt.Fprintf(os.Stdout, "mamori version %s\n", version)
		return 0
	case "explain":
		return explainCmd(args[1:], os.Stdout, os.Stderr)
	case "schema":
		return schemaCmd(args[1:], os.Stdout, os.Stderr)
	case "policy":
		return policyCmd(args[1:], os.Stdout, os.Stderr)
	case "diff":
		return diffCmd(args[1:], os.Stdout, os.Stderr)
	case "vet":
		return vetCmd(args[1:], os.Stdout, os.Stderr)
	case "doctor":
		return doctorCmd(args[1:], os.Stdout, os.Stderr)
	case "status":
		return statusCmd(args[1:], os.Stdout, os.Stderr)
	default:
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, usage)
}
