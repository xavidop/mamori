package mamoriprov

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xavidop/mamori"
)

const (
	// watchChannelBuffer sizes the channel Watch returns. A small buffer
	// lets a burst of a couple of frames (e.g. an error frame immediately
	// followed by a fresh value on reconnect) land without the read loop
	// blocking on a slow consumer, without holding an unbounded backlog.
	watchChannelBuffer = 8

	// watchBackoffFloor is the reconnect wait after the very first
	// connection attempt, and what the backoff resets to once a stream has
	// been successfully established (see watchLoop).
	watchBackoffFloor = 100 * time.Millisecond
	// watchBackoffCap bounds exponential growth of the reconnect wait so a
	// persistently unreachable server is retried at most this infrequently.
	watchBackoffCap = 30 * time.Second

	// sseScanInitialBuf is bufio.Scanner's starting buffer size; it grows as
	// needed up to sseScanMaxLine.
	sseScanInitialBuf = 4 * 1024
	// sseScanMaxLine bounds a single SSE line. Without a bound, a hostile or
	// broken server could stream one line with no newline forever and grow
	// the scanner's internal buffer without limit; past this size Scan
	// fails with bufio.ErrTooLong instead, which ends the read loop and
	// triggers the normal reconnect-with-backoff path.
	sseScanMaxLine = 1 << 20 // 1 MiB

	// sseMaxFrameBytes bounds the TOTAL accumulated size of one frame's
	// data: lines. sseScanMaxLine only bounds a single line; without a
	// separate per-frame cap, a hostile or broken server could keep sending
	// data: lines and never a blank line to dispatch the frame, growing
	// dataLines without bound (OOM) even though every individual line stays
	// under sseScanMaxLine. Past this size the frame is treated as a stream
	// error, the same way a read error is: readSSE stops reading this
	// connection so watchLoop reconnects with backoff, rather than
	// delivering a truncated or oversized frame.
	sseMaxFrameBytes = 1 << 20 // 1 MiB total per frame
)

// Watch implements mamori.WatchableProvider by opening a persistent
// GET /v1/watch?name=<name> Server-Sent-Events connection to the mamori
// config server and forwarding every "update"/"error" frame it sends as a
// mamori.Update, reconnecting with backoff whenever the connection drops.
// This makes mamori:// a NATIVE watch: mamori must never wrap this Provider
// in its own polling adapter.
//
// The binding name is ref.Path, exactly as for Resolve and ResolveBatch (see
// Resolve's doc comment for why a mamori:// ref's path is always a binding
// name). An empty name is rejected synchronously, before any goroutine
// starts, since Watch can still return an error at this point.
//
// Once past that check, Watch always succeeds: it returns a buffered
// channel immediately and starts exactly one goroutine (watchLoop) that owns
// the channel for its entire lifetime. That goroutine is the only writer to
// the channel and the only place it is closed - across every reconnect the
// same channel keeps being written to, and it is closed exactly once, when
// ctx is done (see watchLoop).
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	name := ref.Path
	if name == "" {
		return nil, fmt.Errorf("%w: mamori:// ref %q has no binding name", mamori.ErrInvalid, ref.Raw)
	}

	ch := make(chan mamori.Update, watchChannelBuffer)
	go p.watchLoop(ctx, name, ch)
	return ch, nil
}

// watchLoop owns ch for its entire lifetime: it is the only goroutine that
// ever writes to ch, and its single `defer close(ch)` is the only place ch
// is ever closed - never across a reconnect, only when ctx is done (directly,
// or because a blocked send below observed ctx.Done() instead).
//
// Each iteration calls watchOnce, which owns one HTTP connection end to end:
// connect, stream frames until the connection ends for any reason, and
// return whether a stream was ever successfully established. backoff is the
// reconnect wait, exponential from watchBackoffFloor and capped at
// watchBackoffCap, WITH jitter (watchBackoffJitter) so many clients
// reconnecting to the same server at once do not all retry in lockstep. It
// resets to watchBackoffFloor immediately after any successfully-established
// stream (established == true), regardless of how that stream eventually
// ended: a connection that stayed up for an hour and then dropped deserves a
// fast retry, not a wait grown from a run of earlier failures that have
// nothing to do with the server's current health.
func (p *Provider) watchLoop(ctx context.Context, name string, ch chan mamori.Update) {
	defer close(ch)

	backoff := watchBackoffFloor
	for {
		if ctx.Err() != nil {
			return
		}

		established := p.watchOnce(ctx, name, ch)
		if ctx.Err() != nil {
			return
		}

		if established {
			backoff = watchBackoffFloor
		}

		select {
		case <-time.After(watchBackoffJitter(backoff)):
		case <-ctx.Done():
			return
		}
		backoff = nextWatchBackoff(backoff)
	}
}

// watchOnce owns one HTTP connection attempt and, if it succeeds, the
// resulting SSE stream: it issues GET /v1/watch?name=<name>, classifies a
// failure to even establish the stream (a do error, meaning the server could
// not be reached at all, or a non-200 status, meaning the server answered
// but refused the subscription) as a transient mamori.Update sent on ch, and
// otherwise reads frames until the connection ends for any reason (EOF, a
// read error, or ctx cancellation).
//
// It returns established = true once the server has answered 200 and the
// SSE stream has actually started being read, regardless of how that stream
// later ends. A post-connect disconnect (EOF, a read error) is NOT itself
// reported as an Update - see readSSE's doc comment for why - it only ends
// this call so watchLoop can back off and reconnect.
func (p *Provider) watchOnce(ctx context.Context, name string, ch chan<- mamori.Update) (established bool) {
	q := url.Values{}
	q.Set("name", name)

	resp, err := p.do(ctx, http.MethodGet, "/v1/watch?"+q.Encode(), nil, func(req *http.Request) {
		req.Header.Set("Accept", "text/event-stream")
	})
	if err != nil {
		// do failed before any response was received at all - a dial
		// refused, a TLS handshake failure, DNS, or similar. There is no
		// server-supplied kind to classify this from, so it is reported as
		// mamori.ErrUnavailable: the honest characterization of "the server
		// could not be reached," as distinct from the classified wire
		// errors below.
		sendUpdate(ctx, ch, mamori.Update{
			Err: fmt.Errorf("%w: connecting mamori watch for %q: %s", mamori.ErrUnavailable, name, err),
		})
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		sendUpdate(ctx, ch, mamori.Update{Err: watchConnectError(name, resp)})
		return false
	}

	// Unblock a blocked Scan on ctx cancellation: closing the response body
	// makes the scanner's in-flight (or next) Read on it return immediately
	// with an error. This is the explicit, deterministic half of ctx
	// cancellation support; http.NewRequestWithContext's own request
	// context (used by p.do) would eventually have the transport do the
	// same, but closing the body directly here does not depend on that
	// transport-specific timing. stop cancels the callback once this
	// function returns by any other path (a normal EOF, say) so it never
	// fires - and therefore never leaks a goroutine - after the stream it
	// would have torn down is already gone.
	stop := context.AfterFunc(ctx, func() { resp.Body.Close() })
	defer stop()

	readSSE(ctx, resp.Body, name, ch)
	return true
}

// watchConnectError classifies a non-200 GET /v1/watch response the same
// way Resolve classifies a non-200 GET /v1/values/{name} response: decode
// the errorEnvelope body and, if its kind maps to a sentinel, wrap that;
// otherwise report a bare, unclassified error. This is deliberately NOT
// forced to mamori.ErrUnavailable the way a do failure is (see watchOnce):
// a non-200 here is a real, classified answer from the server (401
// unauthenticated, 403 forbidden, 400 a missing name parameter, ...), and
// collapsing all of those into ErrUnavailable would throw away information
// a caller can otherwise act on (e.g. distinguishing "retry later" from
// "fix your credentials").
func watchConnectError(name string, resp *http.Response) error {
	limited := io.LimitReader(resp.Body, errBodyLimit)
	var env errorEnvelope
	if err := json.NewDecoder(limited).Decode(&env); err != nil {
		return fmt.Errorf("mamori: watch request for %q returned status %d with an undecodable body: %s", name, resp.StatusCode, err)
	}
	if sentinel := sentinelForKind(env.Error.Kind); sentinel != nil {
		return fmt.Errorf("%w: %s", sentinel, env.Error.Message)
	}
	return fmt.Errorf("mamori: watch request for %q returned status %d kind %q: %s", name, resp.StatusCode, env.Error.Kind, env.Error.Message)
}

// readSSE reads SSE frames from body until the stream ends for any reason
// (EOF, a read error, an over-long line, or ctx cancellation closing body
// out from under it - see watchOnce), dispatching each complete frame via
// dispatchSSEFrame as it is parsed.
//
// Parsing follows the SSE spec's line-based framing: "event:" and "data:"
// field lines accumulate into the current frame, a blank line dispatches
// and resets it, and a line starting with ":" is a comment (the server's
// heartbeat - see server/wire.go's writeSSEHeartbeat) that carries no field
// at all and is ignored outright, never touching the accumulator. A single
// optional leading space after the field's colon is trimmed per the spec;
// any other field name (there are none on this wire today, but "id" and
// "retry" are legal SSE fields) is accumulated into nothing and effectively
// ignored.
//
// A disconnect here - the scanner returning false, however that happened -
// is NOT itself sent to ch as an Update. SSE streams are expected to drop
// and get re-established periodically (idle proxies, server restarts, a
// deliberate connection-per-poll-cycle server implementation); surfacing
// every such disconnect as a transient error would make the channel noisy
// with events a caller can do nothing useful about. watchLoop's backoff
// already ensures reconnects do not hammer the server, which is the actual
// risk a disconnect poses.
//
// A frame whose accumulated data: lines exceed sseMaxFrameBytes before a
// blank line dispatches it is handled the same way: reading stops and the
// (incomplete) frame is discarded rather than delivered, so the connection
// tears down and watchLoop reconnects with backoff instead of the goroutine
// hanging onto an ever-growing dataLines slice forever.
func readSSE(ctx context.Context, body io.Reader, name string, ch chan<- mamori.Update) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, sseScanInitialBuf), sseScanMaxLine)

	var event string
	var dataLines []string
	var frameBytes int

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if event != "" || dataLines != nil {
				if !dispatchSSEFrame(ctx, name, event, dataLines, ch) {
					return // ctx is done; sendUpdate already observed it
				}
			}
			event, dataLines, frameBytes = "", nil, 0

		case strings.HasPrefix(line, ":"):
			// Heartbeat/comment line: no field, deliberately not touching
			// the accumulator, so the blank line that follows it dispatches
			// nothing.

		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				frameBytes += len(value)
				if frameBytes > sseMaxFrameBytes {
					// Stream error: this frame has grown past the
					// per-frame cap without a blank line to dispatch it.
					// Stop reading this connection now, the same way an
					// EOF or read error would end the loop, instead of
					// delivering a truncated frame or growing dataLines
					// without bound. watchLoop reconnects with backoff.
					return
				}
				dataLines = append(dataLines, value)
			}
		}
	}
}

// dispatchSSEFrame decodes one complete SSE frame (event name plus its
// joined data lines) and sends the corresponding mamori.Update on ch,
// reporting whether the caller should keep reading (false means ctx is
// done and the read loop should stop).
//
// Only "update" and "error" are recognized, matching exactly what the
// server ever sends (server/wire.go's writeSSEValue and writeSSEError both
// always set an explicit event line; the heartbeat comment never reaches
// here at all - see readSSE). A frame with any other event name (including
// the empty string, e.g. from a malformed or future server) is silently
// dropped rather than guessed at, per this package's convention (see
// sentinelForKind's doc comment) of never fabricating meaning the server
// did not actually send. A frame whose data fails to decode is dropped the
// same way, rather than tearing down an otherwise-healthy stream over one
// bad frame.
func dispatchSSEFrame(ctx context.Context, name, event string, dataLines []string, ch chan<- mamori.Update) bool {
	data := strings.Join(dataLines, "\n")

	switch event {
	case "update":
		var vb valueBody
		if err := json.Unmarshal([]byte(data), &vb); err != nil {
			return true
		}
		var notAfter time.Time
		if vb.NotAfter != nil {
			notAfter = *vb.NotAfter
		}
		return sendUpdate(ctx, ch, mamori.Update{Value: mamori.Value{
			Bytes:     vb.Bytes,
			Version:   vb.Version,
			Sensitive: vb.Sensitive,
			NotAfter:  notAfter,
			Metadata:  vb.Metadata,
		}})

	case "error":
		var vb valueBody
		if err := json.Unmarshal([]byte(data), &vb); err != nil || vb.Error == nil {
			return true
		}
		var watchErr error
		if sentinel := sentinelForKind(vb.Error.Kind); sentinel != nil {
			watchErr = fmt.Errorf("%w: %s", sentinel, vb.Error.Message)
		} else {
			watchErr = fmt.Errorf("mamori: watch error kind %q for %q: %s", vb.Error.Kind, name, vb.Error.Message)
		}
		return sendUpdate(ctx, ch, mamori.Update{Err: watchErr})

	default:
		return true
	}
}

// sendUpdate sends u on ch, or reports false without sending if ctx is done
// first. Every send in this file goes through this helper so ctx
// cancellation can never leave the goroutine blocked forever on a full
// channel nobody is reading anymore.
func sendUpdate(ctx context.Context, ch chan<- mamori.Update, u mamori.Update) bool {
	select {
	case ch <- u:
		return true
	case <-ctx.Done():
		return false
	}
}

// nextWatchBackoff doubles d, capped at watchBackoffCap. The d <= 0 check
// guards the (practically unreachable, given the cap) case of d doubling
// past time.Duration's max representable value and wrapping negative.
func nextWatchBackoff(d time.Duration) time.Duration {
	d *= 2
	if d <= 0 || d > watchBackoffCap {
		d = watchBackoffCap
	}
	return d
}

// watchBackoffJitter applies "equal jitter" to d, returning a value drawn
// uniformly from [d/2, d]. This keeps the wait close to d (unlike "full
// jitter", [0, d]) while still spreading many clients' reconnect attempts
// out instead of letting them retry in lockstep.
func watchBackoffJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}
