// This file implements `mamori diff`: it compares two `mamori explain --json`
// outputs and reports what changed about a service's configuration surface,
// including the delta in backend permissions that surface implies.
//
// It is a static command (decision D1: the CLI's static commands read source
// and never resolve anything), but the only one that does not even read Go
// source: its inputs are two JSON files, so it needs no build, no module
// graph, and no network, which is what lets it run against a base commit
// that no longer builds.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

var diffUsage = `usage: mamori diff <base.json> <head.json> [--json|--markdown]
                   [--exit-code[=any|privilege]] [--policy-format=<f>]

Diff compares two "mamori explain --json" outputs and reports what changed
about a config surface: fields added, removed, and modified, precedence
chains gained and lost, and the backend paths the service newly reads or
stops reading. It never resolves anything (no network calls, no secret
managers contacted) and never loads Go source: both operands are JSON files.

  <base.json>       the earlier explain output. "-" reads it from stdin.
  <head.json>       the later explain output. "-" reads it from stdin.
                    At most one operand may be "-".
  --json            emit the whole diff as JSON instead of a text report
  --markdown        emit markdown suited to a pull request comment or
                    $GITHUB_STEP_SUMMARY. Mutually exclusive with --json.
  --exit-code       signal findings through the exit code (see below).
                    A bare --exit-code means "any".
  --policy-format   additionally render the concrete grant for each changed
                    backend path, in one of: aws-iam, gcp, external-secret.
                    Without it the privilege delta is scheme neutral, which
                    works for every provider and presumes no cloud.

Typical CI use:

  git checkout "$BASE" && mamori explain ./... --json > /tmp/base.json
  git checkout "$HEAD" && mamori explain ./... --json > /tmp/head.json
  mamori diff /tmp/base.json /tmp/head.json --markdown >> "$GITHUB_STEP_SUMMARY"

  # and, to block a merge that grows the permission surface:
  mamori diff /tmp/base.json /tmp/head.json --exit-code=privilege

Exit codes:
  0   success. Also the result when findings exist but --exit-code was not
      given: showing a diff is not a failure.
  1   usage error, unreadable operand, or JSON that is not an explain output
  2   findings present, and --exit-code asked for them to be signalled.
      --exit-code=any signals on any change; --exit-code=privilege signals
      only when the permission surface GREW, since losing access is not a
      risk worth gating a merge on.
`

// exit-code modes.
const (
	exitCodeOff       = ""
	exitCodeAny       = "any"
	exitCodePrivilege = "privilege"
)

// diffConfig carries the injectable stdin, so the "-" operand is testable
// without touching the real os.Stdin. It mirrors liveFlags.stdin (flags.go).
type diffConfig struct {
	stdin io.Reader
}

// diffCmd is the mamori diff subcommand, using the real stdin.
func diffCmd(args []string, stdout, stderr io.Writer) int {
	c := &diffConfig{}
	return c.run(args, stdout, stderr)
}

func (c *diffConfig) run(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, diffUsage)
	}

	opts, err := parseDiffArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, diffUsage)
		return 1
	}

	base, err := c.loadExplainJSON(opts.baseFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori diff: %v\n", err)
		return 1
	}
	head, err := c.loadExplainJSON(opts.headFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori diff: %v\n", err)
		return 1
	}

	d := computeDiff(base, head)

	switch {
	case opts.jsonOut:
		if code := renderDiffJSON(stdout, stderr, d); code != 0 {
			return code
		}
	case opts.markdown:
		renderDiffMarkdown(stdout, d, opts.policyFormat)
	default:
		renderDiffText(stdout, d, opts.policyFormat)
	}

	switch opts.exitCode {
	case exitCodeAny:
		if !d.Empty() {
			return 2
		}
	case exitCodePrivilege:
		if d.PrivilegeGrew() {
			return 2
		}
	}
	return 0
}

// diffOptions is the parsed command line.
type diffOptions struct {
	baseFile     string
	headFile     string
	jsonOut      bool
	markdown     bool
	exitCode     string
	policyFormat string
}

// parseDiffArgs scans by recognized flag shape rather than using
// flag.FlagSet, so operands and flags may appear in either order, matching
// parseExplainArgs and parsePolicyArgs.
func parseDiffArgs(args []string) (diffOptions, error) {
	var o diffOptions
	var operands []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			o.jsonOut = true
		case a == "--markdown" || a == "-markdown":
			o.markdown = true
		case a == "--exit-code" || a == "-exit-code":
			o.exitCode = exitCodeAny
		case strings.HasPrefix(a, "--exit-code="):
			o.exitCode = strings.TrimPrefix(a, "--exit-code=")
		case strings.HasPrefix(a, "-exit-code="):
			o.exitCode = strings.TrimPrefix(a, "-exit-code=")
		case strings.HasPrefix(a, "--policy-format="):
			o.policyFormat = strings.TrimPrefix(a, "--policy-format=")
		case strings.HasPrefix(a, "-policy-format="):
			o.policyFormat = strings.TrimPrefix(a, "-policy-format=")
		case a == "--policy-format" || a == "-policy-format":
			i++
			if i >= len(args) {
				return diffOptions{}, fmt.Errorf("mamori diff: %s requires a value", a)
			}
			o.policyFormat = args[i]
		case a == "-":
			// The stdin operand, not a flag.
			operands = append(operands, a)
		case strings.HasPrefix(a, "-"):
			return diffOptions{}, fmt.Errorf("mamori diff: unknown flag %q", a)
		default:
			operands = append(operands, a)
		}
	}

	if len(operands) != 2 {
		return diffOptions{}, fmt.Errorf("mamori diff: need exactly two operands (base and head), got %d", len(operands))
	}
	o.baseFile, o.headFile = operands[0], operands[1]
	if o.baseFile == "-" && o.headFile == "-" {
		return diffOptions{}, fmt.Errorf("mamori diff: only one operand may be \"-\"")
	}

	if o.jsonOut && o.markdown {
		return diffOptions{}, fmt.Errorf("mamori diff: --json and --markdown are mutually exclusive")
	}

	switch o.exitCode {
	case exitCodeOff, exitCodeAny, exitCodePrivilege:
	default:
		return diffOptions{}, fmt.Errorf("mamori diff: unknown --exit-code value %q (want any or privilege)", o.exitCode)
	}

	switch o.policyFormat {
	case "", formatAWSIAM, formatGCP, formatExternalSecret:
	default:
		return diffOptions{}, fmt.Errorf("mamori diff: unknown --policy-format %q (want %s)",
			o.policyFormat, strings.Join(supportedPolicyFormats, ", "))
	}

	return o, nil
}

// loadExplainJSON reads one operand and decodes it as an explain output.
//
// Decoding is deliberately tolerant of unknown object fields (the default for
// encoding/json, stated here because it is load-bearing rather than
// incidental): base.json is typically a stored CI artifact produced weeks
// earlier, possibly by an older mamori binary, so a newer binary's output must
// stay readable by an older diff and vice versa. See the stability promise
// documented on `mamori explain --json`: fields may be added to that JSON,
// never removed and never retyped.
//
// A top-level value that is not an array is rejected by name, since the
// likeliest cause is a file produced by a different command.
func (c *diffConfig) loadExplainJSON(path string) ([]StructInfo, error) {
	var r io.Reader
	if path == "-" {
		r = c.stdin
		if r == nil {
			r = os.Stdin
		}
	} else {
		fh, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = fh.Close() }()
		r = fh
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var out []StructInfo
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s is not a \"mamori explain --json\" output: %w", path, err)
	}
	return out, nil
}
