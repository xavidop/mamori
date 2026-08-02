// This file implements `mamori doctor`: a thin client of a running
// process's admin endpoint (decision D1's live half; see client.go for the
// GET / classification this command reports). Unlike explain/schema/policy,
// doctor reads no source by default; --compare is the one place a live
// command also touches source, diffing the live report's field set against
// what Extract finds in a source tree.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

const doctorUsage = `usage: mamori doctor [--endpoint=<url>] [flags]

Doctor GETs / on a running process's admin endpoint (see mamori.WithAdminHTTP
/ mamori.Handler) and reports its health. It never reads source and never
resolves anything on its own; it only renders whatever mamori.Report the
target process's admin endpoint returns.

Endpoint flags (shared with status; see also --bearer/--bearer-file,
--basic/--basic-file, --client-cert/--client-key):
  --endpoint    admin endpoint URL: https://host:port, http://host:port
                (only with --insecure), or unix:///path/to.sock
  --insecure    allow a plaintext http:// endpoint

Doctor-specific flags:
  --json        emit the admin endpoint's raw response body unchanged,
                instead of a table
  --compare     space-separated Go package patterns; statically extract
                source: refs from them (like "mamori explain") and flag any
                field present in the source but missing from the live
                report, or present live but not found in source (drift)
  --secret-schemes
                comma-separated extra schemes to treat as secret-bearing,
                added to the built-in set. Only affects --compare, which is
                the only part of doctor that reads source.

Exit codes:
  0   the target process is healthy
  1   the target process is reachable but unhealthy
  2   reachable, but not a usable mamori admin Report (404, or a 200 body
      that does not decode as one -- admin API off, or not a mamori
      process; configure mamori.WithAdminHTTP)
  3   never got an HTTP response (connection refused, no such socket, TLS
      failure, or a malformed --endpoint)
  4   reachable mamori admin API, but authentication failed (401)
`

// doctorCmd is the mamori doctor subcommand. It writes its output to stdout
// and any errors to stderr (both injected so tests never touch the real
// os.Stdout/os.Stderr), and returns fetchReport's exit code (see
// client.go): --compare's drift, if any, is reported alongside the normal
// output but does not change the returned code, since a source/live
// mismatch is a separate concern from the target process's own health.
func doctorCmd(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, doctorUsage)
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, doctorUsage) }

	var lf liveFlags
	lf.register(fs)
	jsonOut := fs.Bool("json", false, "emit the raw admin Report JSON body unchanged")
	compare := fs.String("compare", "", "space-separated Go package patterns to compare against the live report")
	// --compare is the only part of doctor that reads source, so it is the
	// only part that needs the scheme set (see secretschemes.go). Registering
	// the flag here keeps the spelling identical to the static commands.
	secretSchemes := fs.String("secret-schemes", "", "comma-separated extra schemes to treat as secret-bearing, added to the built-in set (only affects --compare)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Validate eagerly, before any network call: a typo in the scheme list
	// should fail immediately rather than after the endpoint round trip.
	schemes, err := secretSchemeSet("doctor", *secretSchemes)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	res := doFetch(context.Background(), lf)

	if *jsonOut {
		writeDoctorJSON(stdout, res)
	} else {
		if res.rep != nil {
			writeReportTable(stdout, res.rep)
		}
	}
	if res.err != nil {
		_, _ = fmt.Fprintln(stderr, res.err)
	}

	if *compare != "" {
		runCompare(stdout, stderr, strings.Fields(*compare), res.rep, schemes)
	}

	return res.exit
}

// writeDoctorJSON writes res.body verbatim (never re-marshaled: what the
// admin endpoint sent is exactly what this prints, byte for byte) when a
// response body was received at all. When the endpoint was never reached
// (res.body is nil, exit 3), there is nothing to print to stdout; the error
// on stderr already explains why.
func writeDoctorJSON(stdout io.Writer, res fetchResult) {
	if res.body == nil {
		return
	}
	_, _ = stdout.Write(res.body)
	if len(res.body) == 0 || res.body[len(res.body)-1] != '\n' {
		_, _ = fmt.Fprintln(stdout)
	}
}

// writeReportTable renders rep for a human: one row per field (Path,
// Scheme, Ref, Version, Stale, LastKind, LastError, Sensitive, Derived)
// followed by a summary line. status.go's statusCmd reuses this so `doctor`
// and `status` render an identical table for the same report.
//
// A Derived row (WithDerive declared it as a write path; see
// mamori.FieldStatus.Derived) always renders SCHEME and REF blank: it has no
// ref, so there is nothing there to show. VERSION carries a content hash of
// the computed value (see mamori.FieldStatus.Version, derivedVersion in
// derivedversion.go); a blank VERSION on a derived row means the value was
// never evaluated - a Doctor probe whose source field produced no value to
// feed the hooks, whose hook itself failed, or whose hooks could not be typed
// to the config at all (LAST_KIND reads invalid for the latter two). The DERIVED
// column is what tells a reader that is expected - a field mamori maintains
// but never resolved from anywhere - rather than a row that looks like a
// misconfigured or half-broken source field.
func writeReportTable(stdout io.Writer, rep *mamori.Report) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PATH\tSCHEME\tREF\tVERSION\tSTALE\tLAST_KIND\tLAST_ERROR\tSENSITIVE\tDERIVED")
	for _, f := range rep.Fields {
		lastKind := string(f.LastKind)
		if lastKind == "" {
			lastKind = "-"
		}
		lastErr := f.LastError
		if lastErr == "" {
			lastErr = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Path, f.Scheme, f.Ref, f.Version,
			strconv.FormatBool(f.Stale), lastKind, lastErr, strconv.FormatBool(f.Sensitive),
			strconv.FormatBool(f.Derived))
	}
	_ = tw.Flush()

	status := "HEALTHY"
	if !rep.Healthy {
		status = "UNHEALTHY"
	}
	pinned := ""
	if rep.Pinned {
		pinned = ", pinned"
	}
	_, _ = fmt.Fprintf(stdout, "\n%s: %d field(s), snapshot %d (live %d)%s, generated %s\n",
		status, len(rep.Fields), rep.Snapshot, rep.Live, pinned,
		rep.GeneratedAt.Format(time.RFC3339))
}

// runCompare implements --compare: it statically extracts every
// source-tagged field path from patterns (the same walk explain uses, via
// Extract), and diffs that set against rep's live field paths, printing any
// field present in exactly one of the two. rep may be nil (the fetch never
// produced one, e.g. an unreachable endpoint), in which case every source
// field is reported as missing from live: --compare still runs and still
// reports what it can, rather than silently doing nothing just because the
// live half of the comparison came back empty.
//
// A Derived live field (mamori.FieldStatus.Derived) is excluded from the live
// set entirely, not merely tolerated as a mismatch: Extract only ever walks
// `source:`-tagged struct fields (sourcetag.go), and a WithDerive-declared
// write path carries no `source` tag by construction, so it can never appear
// on the source side of this comparison no matter how correctly it is
// configured. Without this exclusion, every process that declares a derived
// field would report "only in live (not source)" for it on every single
// --compare run - permanent, unfixable drift for a field that is working
// exactly as intended.
func runCompare(stdout, stderr io.Writer, patterns []string, rep *mamori.Report, schemes sourcetag.SchemeSet) {
	structs, err := Extract(patterns, "", schemes)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori doctor --compare: %v\n", err)
		return
	}

	source := map[string]bool{}
	for _, s := range structs {
		for _, f := range s.Fields {
			source[f.Path] = true
		}
	}
	live := map[string]bool{}
	if rep != nil {
		for _, f := range rep.Fields {
			if f.Derived {
				continue
			}
			live[f.Path] = true
		}
	}

	var onlySource, onlyLive []string
	for p := range source {
		if !live[p] {
			onlySource = append(onlySource, p)
		}
	}
	for p := range live {
		if !source[p] {
			onlyLive = append(onlyLive, p)
		}
	}
	sort.Strings(onlySource)
	sort.Strings(onlyLive)

	_, _ = fmt.Fprintln(stdout, "\ncompare: source vs. live field paths")
	if len(onlySource) == 0 && len(onlyLive) == 0 {
		_, _ = fmt.Fprintln(stdout, "  no drift: source and live field sets match")
		return
	}
	for _, p := range onlySource {
		_, _ = fmt.Fprintf(stdout, "  only in source (not live): %s\n", p)
	}
	for _, p := range onlyLive {
		_, _ = fmt.Fprintf(stdout, "  only in live (not source): %s\n", p)
	}
}
