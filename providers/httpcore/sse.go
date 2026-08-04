package httpcore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/xavidop/mamori"
)

// DefaultSSEMaxLine is the ceiling on a single Server-Sent-Events line applied
// when [SSEConfig.MaxLine] is not positive.
//
// The bound exists because an SSE line has no length prefix: a line ends when a
// newline arrives, and nothing obliges a server to ever send one. A hostile or
// merely broken backend can stream a single line forever, and a decoder that
// reads until the newline grows its buffer for as long as that lasts. One
// megabyte is far past any real frame on the wires this package serves (a
// Realtime Database put, a mamori config value) while still being a number a
// process can absorb.
const DefaultSSEMaxLine = 1 << 20

// DefaultSSEMaxFrame is the ceiling on the TOTAL accumulated data of one
// Server-Sent-Events frame, applied when [SSEConfig.MaxFrame] is not positive.
//
// This is a SECOND bound, and it is not redundant with [DefaultSSEMaxLine].
// Bounding a line only bounds one line. A frame is dispatched by a blank line,
// and a server that sends "data: x" a million times and never a blank line has
// broken no line bound at all while the decoder holds every one of those lines
// waiting for a frame that never completes. Per-line bounding alone therefore
// still permits unbounded growth; only a per-frame total closes it.
const DefaultSSEMaxFrame = 1 << 20

// sseInitialBuf is the decoder's starting read buffer. It grows on demand up to
// the configured line bound, so this only decides how many small allocations a
// stream of ordinary frames performs, not what it is allowed to reach.
const sseInitialBuf = 4 * 1024

// ErrSSELineTooLong reports that one line exceeded [SSEConfig.MaxLine].
//
// It is classified as mamori.ErrUnavailable, not mamori.ErrInvalid, and the
// difference is load bearing: mamori's fieldUnhealthy treats KindInvalid as a
// terminal fault that marks the field unhealthy until a human intervenes, while
// KindUnavailable is a kind mamori expects to heal on its own. A server that
// emits one over-long line is a stream to tear down and re-establish, which is
// what every consumer of this package does with it, so the kind must be the
// retryable one.
var ErrSSELineTooLong = fmt.Errorf("httpcore: SSE line exceeds the configured limit: %w", mamori.ErrUnavailable)

// ErrSSEFrameTooLong reports that one frame's accumulated data exceeded
// [SSEConfig.MaxFrame] before a blank line dispatched it. It carries
// mamori.ErrUnavailable for the same reason [ErrSSELineTooLong] does.
var ErrSSEFrameTooLong = fmt.Errorf("httpcore: SSE frame exceeds the configured limit: %w", mamori.ErrUnavailable)

// SSEEvent is one dispatched Server-Sent-Events frame.
type SSEEvent struct {
	// Name is the frame's "event" field, empty when the frame carried none.
	Name string
	// Data is the frame's "data" fields joined with "\n", per the SSE spec.
	//
	// It is freshly allocated for every frame and is never reused by the
	// decoder, so a caller may retain it past the next call to Next without
	// copying. A decoder that recycled one buffer would be faster and would
	// silently corrupt any consumer that keeps a frame around, which both of
	// this package's SSE consumers do while they decode JSON out of it.
	Data []byte
}

// SSEConfig bounds an SSE decoder. The zero value selects both defaults and is
// the right choice unless a backend is known to send larger frames.
type SSEConfig struct {
	// MaxLine bounds a single line. Not positive selects DefaultSSEMaxLine.
	MaxLine int
	// MaxFrame bounds one frame's total accumulated data, including the
	// newlines that join multiple data fields. Not positive selects
	// DefaultSSEMaxFrame.
	MaxFrame int
}

// SSEDecoder parses a Server-Sent-Events byte stream into discrete frames under
// a memory bound.
//
// It is deliberately independent of net/http: it reads from any io.Reader, so
// the parsing and, more importantly, the two bounds can be tested directly
// against a strings.Reader rather than through a server. [SSEStream] is the thin
// net/http binding on top of it.
//
// It starts no goroutine. A streaming decoder is the easiest place in a provider
// to leak one, and the way this package avoids that is by never having one to
// leak: everything here happens on the caller's goroutine, and cancellation is
// the caller's business (again, see [SSEStream]).
//
// It is not safe for concurrent use; one stream is read by one goroutine.
type SSEDecoder struct {
	sc       *bufio.Scanner
	maxLine  int
	maxFrame int
}

// NewSSEDecoder returns a decoder reading frames from r under cfg's bounds.
func NewSSEDecoder(r io.Reader, cfg SSEConfig) *SSEDecoder {
	maxLine := cfg.MaxLine
	if maxLine <= 0 {
		maxLine = DefaultSSEMaxLine
	}
	maxFrame := cfg.MaxFrame
	if maxFrame <= 0 {
		maxFrame = DefaultSSEMaxFrame
	}

	sc := bufio.NewScanner(r)
	// The scanner is allowed one byte past the bound so that a line of exactly
	// maxLine bytes is a token it can return rather than one it rejects, and the
	// starting buffer never exceeds that ceiling: a caller who bounds lines at
	// 64 bytes must not have 4 KiB allocated on their behalf.
	initial := sseInitialBuf
	if initial > maxLine+1 {
		initial = maxLine + 1
	}
	sc.Buffer(make([]byte, initial), maxLine+1)

	return &SSEDecoder{sc: sc, maxLine: maxLine, maxFrame: maxFrame}
}

// Next returns the next complete frame.
//
// Framing follows the SSE spec, matching what both of this package's consumers
// already did by hand: "event" names the frame, "data" accumulates (multiple
// data fields are joined with "\n"), a blank line dispatches whatever has
// accumulated, a line beginning with ":" is a comment (an SSE heartbeat) that is
// ignored outright without touching the accumulator, and every other field name,
// "id" and "retry" among them, is read and discarded. A single optional space
// after a field's colon is trimmed. A line with no colon at all is a field with
// an empty value, per the spec.
//
// A frame is dispatched when a blank line follows at least one field line, so a
// stream of nothing but comments and blank lines yields nothing rather than a
// run of empty frames.
//
// It returns io.EOF when the stream ends cleanly. A frame left incomplete at EOF
// is DISCARDED rather than dispatched, which is what the spec requires: a
// truncated frame is not a short frame, it is a frame whose remaining bytes the
// caller never saw, and delivering it would hand a consumer a half-read JSON
// payload as though the server had finished writing it.
//
// It returns [ErrSSELineTooLong] or [ErrSSEFrameTooLong] when a bound is
// crossed, and the underlying reader's error verbatim otherwise, so a caller can
// still errors.Is it against whatever the transport reports.
func (d *SSEDecoder) Next() (SSEEvent, error) {
	var (
		name      string
		data      []byte
		haveField bool
		haveData  bool
	)

	for d.sc.Scan() {
		line := d.sc.Bytes()
		// Checked explicitly rather than relying on the scanner alone. The
		// scanner only reports a too-long token once its buffer is full, so a
		// line under the starting buffer size but over a small configured bound
		// would otherwise slip through as an ordinary token.
		if len(line) > d.maxLine {
			return SSEEvent{}, ErrSSELineTooLong
		}

		switch {
		case len(line) == 0:
			if haveField {
				return SSEEvent{Name: name, Data: data}, nil
			}
			// Nothing has accumulated: a leading, duplicate, or
			// post-heartbeat blank line dispatches nothing.

		case line[0] == ':':
			// Comment or heartbeat. It carries no field, so it must not set
			// haveField, or the blank line after it would dispatch an empty
			// frame the server never sent.

		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				name = string(value)
				haveField = true
			case "data":
				// The joining newline is counted, so the bound is on what is
				// actually held in memory rather than on the payload with its
				// separators quietly excluded.
				grow := len(value)
				if haveData {
					grow++
				}
				if len(data)+grow > d.maxFrame {
					return SSEEvent{}, ErrSSEFrameTooLong
				}
				if haveData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				haveData = true
				haveField = true
			}
		}
	}

	if err := d.sc.Err(); err != nil {
		// A line that never ends reaches the scanner's ceiling before it
		// reaches the explicit check above, since there is no token to measure.
		if errors.Is(err, bufio.ErrTooLong) {
			return SSEEvent{}, ErrSSELineTooLong
		}
		return SSEEvent{}, err
	}
	return SSEEvent{}, io.EOF
}

// SSEStream reads bounded SSE frames from an HTTP response and ties that
// response's lifetime to a context.
//
// It exists because the parsing half must not know about net/http (see
// [SSEDecoder]) while the cancellation half can only be done with it: a read
// blocked on a socket does not observe a cancelled context, and the only thing
// that unblocks it promptly is closing the response body underneath it. This
// type is that closing, done once and exactly once, whether it is the context or
// the caller that ends the stream.
//
// It starts no goroutine of its own. The context callback it registers runs only
// if the context is actually cancelled before [SSEStream.Close], and Close
// cancels the registration, so nothing is left running after a stream ends.
type SSEStream struct {
	// ctx is retained for the stream's whole lifetime, which is the one case
	// where holding a context in a struct is honest: this stream IS the
	// operation ctx bounds, and Next needs it to tell "the caller cancelled"
	// apart from "the backend broke".
	ctx  context.Context
	resp *http.Response
	dec  *SSEDecoder
	stop func() bool

	once     sync.Once
	closeErr error
}

// NewSSEStream binds resp's body to ctx and returns a stream of bounded frames.
//
// The caller keeps ownership of the request and of status classification: this
// package cannot know which statuses a given backend uses to refuse a
// subscription, and a provider that has already read an error body must not have
// it read again underneath it. Pass only a response whose status the caller has
// accepted.
//
// The caller must Close the returned stream, which is what releases the
// connection back to the transport.
func NewSSEStream(ctx context.Context, resp *http.Response, cfg SSEConfig) *SSEStream {
	s := &SSEStream{
		ctx:  ctx,
		resp: resp,
		dec:  NewSSEDecoder(resp.Body, cfg),
	}
	// The deterministic half of cancellation support. The request context would
	// eventually have the transport abort the read as well, but that timing is
	// transport specific; closing the body here does not depend on it.
	s.stop = context.AfterFunc(ctx, func() { _ = s.closeBody() })
	return s
}

// Next returns the next frame, or ctx.Err() once the stream's context has been
// cancelled.
//
// Reporting the context error rather than whatever the torn-down socket happened
// to produce is what lets a caller tell a clean shutdown from a backend fault. A
// cancelled watch closing its own body surfaces as "use of closed network
// connection" otherwise, which a provider would go on to report as a transient
// backend failure on every single shutdown.
func (s *SSEStream) Next() (SSEEvent, error) {
	ev, err := s.dec.Next()
	if err != nil && s.ctx.Err() != nil {
		return SSEEvent{}, s.ctx.Err()
	}
	return ev, err
}

// Close releases the underlying connection. It is idempotent and safe to call
// concurrently with the context cancellation that may be closing the same body,
// so a caller can defer it unconditionally.
func (s *SSEStream) Close() error {
	// Cancelling the registration first means the common path (a stream ended
	// by EOF or by the caller) never starts the callback's goroutine at all.
	s.stop()
	return s.closeBody()
}

// closeBody performs the single real close shared by Close and the context
// callback. sync.Once is what makes "whoever gets there first" safe: two
// concurrent Close calls on an http response body are not something this package
// should rely on being harmless.
func (s *SSEStream) closeBody() error {
	s.once.Do(func() { s.closeErr = s.resp.Body.Close() })
	return s.closeErr
}
