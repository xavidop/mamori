// This file implements `mamori status`: the same thin admin-endpoint client
// as doctor (client.go), rendered as a one-shot report by default, or
// repeatedly on an interval with --watch. status never touches source: it
// exists purely to watch a running process's own view of its health.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

const statusUsage = `usage: mamori status [--endpoint=<url>] [flags]

Status GETs / on a running process's admin endpoint and renders its health,
the same report doctor renders. With --watch it keeps polling on --interval
until interrupted (Ctrl-C / SIGINT), instead of rendering once and exiting.

Endpoint flags (shared with doctor; see also --bearer/--bearer-file,
--basic/--basic-file, --client-cert/--client-key):
  --endpoint    admin endpoint URL: https://host:port, http://host:port
                (only with --insecure), or unix:///path/to.sock
  --insecure    allow a plaintext http:// endpoint

Status-specific flags:
  --watch       keep polling the admin endpoint until interrupted
  --interval    poll interval when --watch is set (default 2s)

Exit codes:
  Without --watch: the same 0-4 exit codes doctor uses (see "mamori doctor
  --help"). With --watch: 0 once interrupted; the exit code of any single
  poll while watching is only ever printed, never returned, since the point
  of --watch is to keep going through a transient failure.
`

// defaultStatusInterval is --interval's default: frequent enough to notice
// a change quickly, infrequent enough not to hammer the admin endpoint,
// matching the order of magnitude of Watch's own default poll interval for
// non-watchable providers (see reconcile.go's defaultPollInterval).
const defaultStatusInterval = 2 * time.Second

// statusCmd is the mamori status subcommand. It writes to stdout/stderr
// (both injected so tests never touch the real os.Stdout/os.Stderr/signal
// handling implicitly). Without --watch it fetches once and returns
// fetchReport's exit code, exactly like doctorCmd. With --watch it renders
// on every tick until ctx is canceled by SIGINT, then returns 0: the
// operator asked to watch until they said stop, not until any one poll
// failed.
func statusCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, statusUsage) }

	var lf liveFlags
	lf.register(fs)
	watch := fs.Bool("watch", false, "keep polling the admin endpoint until interrupted")
	interval := fs.Duration("interval", defaultStatusInterval, "poll interval when --watch is set")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*watch {
		res := doFetch(context.Background(), lf)
		if res.rep != nil {
			writeReportTable(stdout, res.rep)
		}
		if res.err != nil {
			fmt.Fprintln(stderr, res.err)
		}
		return res.exit
	}

	return watchStatus(stdout, stderr, lf, *interval)
}

// watchStatus renders res on every tick of interval until ctx is canceled
// by SIGINT, rendering once immediately first so the operator is not left
// staring at a blank screen for a full interval before anything appears.
func watchStatus(stdout, stderr io.Writer, lf liveFlags, interval time.Duration) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	render := func() {
		res := doFetch(ctx, lf)
		if res.rep != nil {
			writeReportTable(stdout, res.rep)
		}
		if res.err != nil {
			fmt.Fprintln(stderr, res.err)
		}
	}

	render()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			render()
		}
	}
}
