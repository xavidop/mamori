// This file holds the one definition of per-subcommand help, so that
// `mamori <command> --help` behaves the same everywhere: help text goes to
// stdout, and a successful request exits 0. Asking for help is not an error,
// so a script doing `mamori vet --help` to check the binary works should not
// see a failure, and a user piping help to a pager should not have to
// redirect stderr.
package main

import (
	"fmt"
	"io"
)

// helpFlags are the spellings recognized as "print this command's usage".
// They match what main.go's top-level dispatcher already accepts, so
// `mamori --help` and `mamori vet --help` agree.
var helpFlags = map[string]bool{
	"-h":     true,
	"--help": true,
	"-help":  true,
}

// wantsHelp reports whether args contains an explicit help flag.
//
// It scans every argument rather than only the first because help is worth
// honouring wherever it appears: someone who types `mamori explain ./... -h`
// after seeing an error wants the usage, not a complaint about argument
// order. Checking before any other parsing also means a command prints help
// instead of failing on some OTHER malformed argument in the same line, which
// is the situation in which help is most likely to be wanted.
//
// A bare "--" is not handled specially: no mamori command takes positional
// arguments that could be confused for flags, so there is nothing to escape.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if helpFlags[a] {
			return true
		}
	}
	return false
}

// writeHelp prints a command's usage to stdout and returns the exit code for
// a successful help request. Callers return its result directly, which keeps
// the "help is a success, and goes to stdout" rule in one place instead of
// repeated at six call sites.
func writeHelp(stdout io.Writer, usage string) int {
	_, _ = fmt.Fprint(stdout, usage)
	return 0
}
