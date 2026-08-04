package mamoriprov

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
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
)

// Watch implements mamori.WatchableProvider by opening a persistent
// GET /v1/watch?name=<name> Server-Sent-Events connection to the mamori
// config server and forwarding every "update"/"error" frame it sends as a
// mamori.Update, reconnecting with backoff whenever the connection drops.
// This makes mamori:// a NATIVE watch: mamori must never wrap this Provider
// in its own polling adapter.
//
// With several replicas configured (Config.Endpoints), each reconnect moves
// on to the next endpoint in the list rather than redialing the one that just
// dropped; see watchLoop for the rotation and how it interacts with backoff.
// Because those replicas watch upstream on independent schedules, a reconnect
// can land on one that is a poll cycle behind, so every forwarded update is
// first checked against the newest one already delivered for that binding and
// dropped if it is meaningfully older - see freshnessGuard in freshness.go for
// that guard, what it deliberately does not do, and the clock-skew assumption
// it rests on.
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
//
// Every reconnect ROTATES to the next configured endpoint, round-robin, so a
// replica that dies mid-watch cannot black-hole the watch by being the only
// address this loop ever dials again.
//
// The backoff sleep happens only when that rotation WRAPS, i.e. after a full
// cycle through the list, not between one endpoint and the next. Backoff
// exists to stop a client hammering a server that is struggling, and a
// replica this loop has not tried yet is a different machine that the
// previous one's failure says nothing about; sleeping before dialing it would
// delay failover by (N-1) waits purely to protect a server that was never the
// problem. A 3-replica deployment therefore fails over in three quick dials
// and only then starts backing off, while the single-endpoint case wraps on
// every iteration and so behaves exactly as it always has: connect, sleep,
// grow the backoff, repeat.
func (p *Provider) watchLoop(ctx context.Context, name string, ch chan mamori.Update) {
	defer close(ch)

	// A configuration error leaves p.endpoints empty. The loop still has to
	// run in that case: do reports p.endpointErr from every attempt (it checks
	// that before it so much as looks at the endpoint it is handed), so a
	// misconfigured Watch keeps reporting the error as a transient failure on
	// ch with backoff, exactly as it did before failover existed, instead of
	// silently closing the channel. Hence the single placeholder endpoint,
	// which is never actually dialed.
	endpoints := p.endpoints
	if len(endpoints) == 0 {
		endpoints = []endpoint{{}}
	}

	// The freshness guard is created HERE, outside the loop, and is the one
	// piece of watch state that deliberately OUTLIVES a connection. Resetting
	// it per connection would leave it empty at exactly the moment it is
	// needed: a reconnect rotating onto a laggier replica is the whole reason
	// it exists (see freshnessGuard). It belongs to this goroutine alone and
	// is passed down the call stack rather than stored on the Provider, since
	// two concurrent watches must not share watermarks.
	guard := newFreshnessGuard()

	backoff := watchBackoffFloor
	// current indexes the endpoint this iteration dials; it advances by one,
	// wrapping, after every attempt.
	current := 0
	for {
		if ctx.Err() != nil {
			return
		}

		established := p.watchOnce(ctx, endpoints[current], name, ch, guard)
		if ctx.Err() != nil {
			return
		}

		if established {
			backoff = watchBackoffFloor
		}

		current = (current + 1) % len(endpoints)
		if current != 0 {
			// Mid-cycle: there is an untried replica left, so dial it now
			// rather than sleeping first. See this function's doc comment.
			continue
		}

		select {
		case <-time.After(watchBackoffJitter(backoff)):
		case <-ctx.Done():
			return
		}
		backoff = nextWatchBackoff(backoff)
	}
}

// watchOnce owns one HTTP connection attempt against ONE endpoint (which one
// is watchLoop's rotation decision, not watchOnce's) and, if it succeeds, the
// resulting SSE stream: it issues GET /v1/watch?name=<name>, classifies a
// failure to even establish the stream (a do error, meaning the server could
// not be reached at all, or a non-200 status, meaning the server answered
// but refused the subscription) as a transient mamori.Update sent on ch, and
// otherwise reads frames until the connection ends for any reason (EOF, a
// read error, or ctx cancellation).
//
// Note that a failed attempt is reported on ch here even when another
// endpoint is about to be tried: unlike Resolve, a watch has no single return
// value to hold back until the fleet has been exhausted, and hiding "replica
// A is refusing connections" from the caller would cost them the only signal
// they get that their deployment is degraded.
//
// It returns established = true once the server has answered 200 and the
// SSE stream has actually started being read, regardless of how that stream
// later ends. A post-connect disconnect (EOF, a read error) is NOT itself
// reported as an Update - see readSSE's doc comment for why - it only ends
// this call so watchLoop can rotate on, back off, and reconnect.
//
// guard is the watch's freshness guard, owned by watchLoop and passed through
// untouched: this connection is one of many it outlives, so watchOnce never
// creates, resets, or inspects it.
func (p *Provider) watchOnce(ctx context.Context, ep endpoint, name string, ch chan<- mamori.Update, guard *freshnessGuard) (established bool) {
	q := url.Values{}
	q.Set("name", name)

	resp, err := p.do(ctx, ep, http.MethodGet, "/v1/watch?"+q.Encode(), nil, func(req *http.Request) {
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
	if resp.StatusCode != http.StatusOK {
		// The body belongs to this path: watchConnectError reads the error
		// envelope out of it, and nothing else will ever look at it. On the 200
		// path below the SSE stream takes ownership instead, so there is
		// exactly one closer on each branch rather than a shared defer that
		// closes a body the stream is also responsible for.
		connErr := watchConnectError(name, resp)
		_ = resp.Body.Close()
		sendUpdate(ctx, ch, mamori.Update{Err: connErr})
		return false
	}

	// From here the stream owns the body, including closing it. It also ties
	// the body to ctx, which is what unblocks a read parked on a socket the
	// moment the watch is cancelled: closing the body makes the in-flight (or
	// next) Read return immediately. That is the explicit, deterministic half
	// of ctx cancellation support; http.NewRequestWithContext's own request
	// context (used by p.do) would eventually have the transport do the same,
	// but this does not depend on that transport-specific timing. The stream
	// cancels its own callback registration when it is closed, so nothing is
	// left running - and no goroutine is leaked - once this function returns
	// by any other path, a normal EOF say.
	//
	// The zero SSEConfig selects the shared ceilings, which are the same
	// one-megabyte numbers this file used to define for itself: one on a single
	// line, because a server streaming one line with no newline forever would
	// otherwise grow the read buffer without limit, and a SECOND one on a
	// frame's total accumulated data, because per-line bounding alone still
	// lets a server send data: forever and never the blank line that
	// dispatches. Both live in httpcore now, with the full reasoning, and both
	// apply to every provider that streams rather than only to this one.
	stream := httpcore.NewSSEStream(ctx, resp, httpcore.SSEConfig{})
	defer func() { _ = stream.Close() }()

	readSSE(ctx, stream, name, ch, guard)
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

// readSSE reads SSE frames from stream until it ends for any reason (EOF, a
// read error, an over-long line, an over-large frame, or ctx cancellation
// closing the body out from under it - see watchOnce), dispatching each
// complete frame via dispatchSSEFrame as it is parsed.
//
// The framing itself is httpcore.SSEDecoder's, not this file's, and it is the
// same framing this loop used to implement by hand: "event:" and "data:" field
// lines accumulate into the current frame, a blank line dispatches and resets
// it, a line starting with ":" is a comment (the server's heartbeat - see
// server/wire.go's writeSSEHeartbeat) that carries no field at all and is
// ignored outright, a single optional leading space after the field's colon is
// trimmed, and any other field name ("id" and "retry" are legal SSE fields,
// though there are none on this wire today) is read and discarded. What the
// shared decoder adds is that both memory ceilings now apply to every provider
// that streams, not only to this one; see watchOnce for which ceilings.
//
// A disconnect here - stream.Next returning an error, however that happened -
// is NOT itself sent to ch as an Update. SSE streams are expected to drop
// and get re-established periodically (idle proxies, server restarts, a
// deliberate connection-per-poll-cycle server implementation); surfacing
// every such disconnect as a transient error would make the channel noisy
// with events a caller can do nothing useful about. watchLoop's backoff
// already ensures reconnects do not hammer the server, which is the actual
// risk a disconnect poses.
//
// A frame past either ceiling arrives here as exactly that: an error that ends
// the loop, so the (incomplete) frame is discarded rather than delivered, the
// connection tears down, and watchLoop reconnects with backoff instead of the
// goroutine hanging onto an ever-growing payload forever.
func readSSE(ctx context.Context, stream *httpcore.SSEStream, name string, ch chan<- mamori.Update, guard *freshnessGuard) {
	for {
		ev, err := stream.Next()
		if err != nil {
			return
		}
		if !dispatchSSEFrame(ctx, name, ev.Name, ev.Data, ch, guard) {
			return // ctx is done; sendUpdate already observed it
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
//
// This is also where the freshness guard applies, and only to "update"
// frames: an "error" frame is a live signal about the CURRENT connection, not
// a value, so it can never be out of order and is always forwarded. See
// freshnessGuard for what the guard does with an update.
//
// data is the frame's data: lines already joined with "\n" by the decoder,
// which is where that join moved to; it is not this function's to perform. It
// stays []byte all the way from the decoder into json.Unmarshal: the decoder
// allocates it fresh per frame and nothing here needs a string, so converting
// would copy up to a megabyte per frame for nothing.
func dispatchSSEFrame(ctx context.Context, name, event string, data []byte, ch chan<- mamori.Update, guard *freshnessGuard) bool {
	switch event {
	case "update":
		var vb valueBody
		if err := json.Unmarshal(data, &vb); err != nil {
			return true
		}

		// The guard is keyed on the name the FRAME carries, not the name this
		// call was subscribed with, because one connection can carry frames
		// for several bindings and a lagging one must not hold back the
		// others. A frame that omits its name (no server on this wire does,
		// but a malformed one could) is attributed to the subscribed name,
		// which is the only binding this connection ever asked about.
		frameName := vb.Name
		if frameName == "" {
			frameName = name
		}
		if !guard.allows(frameName, vb.ResolvedAt) {
			// This value predates one already delivered for the same binding:
			// the connection has almost certainly been re-established against
			// a replica that is a poll cycle behind. Drop the frame and keep
			// reading - the stream itself is perfectly healthy, and this
			// replica will send a newer frame once it catches up.
			return true
		}

		var notAfter time.Time
		if vb.NotAfter != nil {
			notAfter = *vb.NotAfter
		}
		// vb.Stale is deliberately not consulted: a last-known-good value
		// being served while upstream is failing is still a real, usable
		// value, and suppressing it would leave the caller with nothing at
		// all. It is dropped here for the same reason vb.Kind is dropped in
		// resolveOnce - mamori.Value has no field to carry it.
		if !sendUpdate(ctx, ch, mamori.Update{Value: mamori.Value{
			Bytes:     vb.Bytes,
			Version:   vb.Version,
			Sensitive: vb.Sensitive,
			NotAfter:  notAfter,
			Metadata:  vb.Metadata,
		}}) {
			return false
		}
		// Recorded only now, after the update has actually reached the
		// caller: a watermark raised by a value nobody received would hide
		// every later value dated before it.
		guard.record(frameName, vb.ResolvedAt)
		return true

	case "error":
		var vb valueBody
		if err := json.Unmarshal(data, &vb); err != nil || vb.Error == nil {
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
