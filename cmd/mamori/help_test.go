package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// subcommands is every subcommand that takes arguments, paired with the
// prefix its usage text starts with. Adding a subcommand without adding it
// here is the drift this table exists to catch.
var subcommands = []struct {
	name  string
	run   func(args []string, stdout, stderr io.Writer) int
	usage string
}{
	{"explain", explainCmd, "usage: mamori explain"},
	{"schema", schemaCmd, "usage: mamori schema"},
	{"policy", policyCmd, "usage: mamori policy"},
	{"vet", vetCmd, "usage: mamori vet"},
	{"doctor", doctorCmd, "usage: mamori doctor"},
	{"status", statusCmd, "usage: mamori status"},
}

// TestHelpIsSuccessOnStdout pins the contract every subcommand shares: asking
// for help is a success, and the help text is the output the user asked for,
// so it goes to stdout and nothing goes to stderr. Before this, the static
// commands called --help an unknown flag (exit 1, stderr) and the live ones
// turned it into a parse failure (exit 2).
func TestHelpIsSuccessOnStdout(t *testing.T) {
	for _, sc := range subcommands {
		for _, flag := range []string{"--help", "-h", "-help"} {
			t.Run(sc.name+"_"+flag, func(t *testing.T) {
				var out, errBuf bytes.Buffer
				code := sc.run([]string{flag}, &out, &errBuf)

				if code != 0 {
					t.Errorf("%s %s = %d, want 0: asking for help is not an error", sc.name, flag, code)
				}
				if !strings.HasPrefix(out.String(), sc.usage) {
					t.Errorf("%s %s stdout does not start with %q:\n%s", sc.name, flag, sc.usage, out.String())
				}
				if errBuf.Len() != 0 {
					t.Errorf("%s %s wrote to stderr, want help on stdout only:\n%s", sc.name, flag, errBuf.String())
				}
			})
		}
	}
}

// TestHelpAnywhereInArgs checks that help is honoured wherever it appears.
// Someone who hits an error and appends -h wants the usage, not a complaint
// about the argument that was already wrong.
func TestHelpAnywhereInArgs(t *testing.T) {
	for _, sc := range subcommands {
		t.Run(sc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			// A deliberately invalid argument precedes the help flag.
			if code := sc.run([]string{"--nope", "--help"}, &out, &errBuf); code != 0 {
				t.Errorf("%s --nope --help = %d, want 0 (help wins over the bad flag)", sc.name, code)
			}
			if !strings.Contains(out.String(), sc.usage) {
				t.Errorf("%s did not print usage to stdout:\n%s", sc.name, out.String())
			}
		})
	}
}

// TestUnknownFlagStillFails guards the other side of the change: making help
// a success must not make a genuinely bad invocation one. Usage on an error
// belongs on stderr, leaving stdout clean for a caller that is piping real
// output somewhere.
func TestUnknownFlagStillFails(t *testing.T) {
	for _, sc := range subcommands {
		t.Run(sc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := sc.run([]string{"--definitely-not-a-flag"}, &out, &errBuf); code == 0 {
				t.Errorf("%s with an unknown flag = 0, want non-zero", sc.name)
			}
			if out.Len() != 0 {
				t.Errorf("%s wrote to stdout on an error, want stderr only:\n%s", sc.name, out.String())
			}
		})
	}
}

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"./...", "--help"}, true},
		{[]string{"--type=Config", "-h", "./..."}, true},
		{nil, false},
		{[]string{"./..."}, false},
		{[]string{"--type=Config"}, false},
		// Not help: a value that merely contains the word.
		{[]string{"--type=help"}, false},
		{[]string{"--secret-schemes", "-h"}, true},
	}
	for _, tt := range tests {
		if got := wantsHelp(tt.args); got != tt.want {
			t.Errorf("wantsHelp(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
