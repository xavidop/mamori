package httpcore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// collect drains dec until it stops returning frames, returning the frames and
// the terminating error.
func collect(t *testing.T, dec *httpcore.SSEDecoder) ([]httpcore.SSEEvent, error) {
	t.Helper()
	var out []httpcore.SSEEvent
	for {
		ev, err := dec.Next()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
		if len(out) > 1000 {
			t.Fatal("decoder produced more than 1000 frames; it is not terminating")
		}
	}
}

func TestSSEDecoderParsesSpecFraming(t *testing.T) {
	// One stream exercising every framing rule the two migrated providers rely
	// on: a named frame, a heartbeat comment, a multi-line data payload, and
	// id/retry fields that must be read and discarded.
	payload := "" +
		"event: put\n" +
		"data: {\"path\":\"/\"}\n" +
		"\n" +
		": heartbeat\n" +
		"\n" +
		"event: update\n" +
		"id: 17\n" +
		"retry: 5000\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"data: no-event-name\n" +
		"\n"

	dec := httpcore.NewSSEDecoder(strings.NewReader(payload), httpcore.SSEConfig{})
	got, err := collect(t, dec)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminating error = %v, want io.EOF", err)
	}

	want := []httpcore.SSEEvent{
		{Name: "put", Data: []byte(`{"path":"/"}`)},
		{Name: "update", Data: []byte("line1\nline2")},
		{Name: "", Data: []byte("no-event-name")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Data) != string(want[i].Data) {
			t.Errorf("frame %d = (%q, %q), want (%q, %q)",
				i, got[i].Name, got[i].Data, want[i].Name, want[i].Data)
		}
	}
}

func TestSSEDecoderIgnoresHeartbeatOnlyStream(t *testing.T) {
	// A comment carries no field, so the blank line after it must dispatch
	// nothing. A decoder that let a heartbeat set "this frame has content"
	// would hand every consumer a run of empty frames to decode.
	dec := httpcore.NewSSEDecoder(strings.NewReader(": ping\n\n: ping\n\n"), httpcore.SSEConfig{})
	got, err := collect(t, dec)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminating error = %v, want io.EOF", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d frames from a heartbeat-only stream, want 0: %q", len(got), got)
	}
}

func TestSSEDecoderDiscardsIncompleteFrameAtEOF(t *testing.T) {
	// No blank line: the server died mid-frame. The bytes so far are not a
	// short frame, they are the start of one, and dispatching them would hand a
	// consumer truncated JSON as though it were complete.
	dec := httpcore.NewSSEDecoder(strings.NewReader("event: update\ndata: {\"half\":"), httpcore.SSEConfig{})
	got, err := collect(t, dec)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminating error = %v, want io.EOF", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d frames from a truncated stream, want 0: %q", len(got), got)
	}
}

func TestSSEDecoderRejectsOverLongLine(t *testing.T) {
	// The per-line bound: one line longer than MaxLine, newline and all.
	payload := "data: " + strings.Repeat("x", 200) + "\n\n"
	dec := httpcore.NewSSEDecoder(strings.NewReader(payload), httpcore.SSEConfig{MaxLine: 64})

	if _, err := dec.Next(); !errors.Is(err, httpcore.ErrSSELineTooLong) {
		t.Fatalf("Next() err = %v, want ErrSSELineTooLong", err)
	}
}

// eofWithData returns its payload and io.EOF from the SAME Read call. io.Reader
// explicitly permits that, and it is what makes the decoder's explicit length
// check on every token necessary rather than decorative: bufio.Scanner hands
// back whatever is buffered as a final token once it has seen EOF, WITHOUT
// consulting its own maximum token size, so the last line of a stream is the one
// place a scanner-only bound does not hold.
type eofWithData struct{ b []byte }

func (e *eofWithData) Read(p []byte) (int, error) {
	n := copy(p, e.b)
	e.b = e.b[n:]
	if len(e.b) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestSSEDecoderBoundsTheFinalLineAtEOF(t *testing.T) {
	// One byte past the bound, unterminated, delivered together with EOF.
	r := &eofWithData{b: []byte("data: " + strings.Repeat("x", 59))}
	if len(r.b) != 65 {
		t.Fatalf("fixture is %d bytes, want MaxLine+1", len(r.b))
	}
	dec := httpcore.NewSSEDecoder(r, httpcore.SSEConfig{MaxLine: 64})

	if _, err := dec.Next(); !errors.Is(err, httpcore.ErrSSELineTooLong) {
		t.Fatalf("Next() err = %v, want ErrSSELineTooLong", err)
	}
}

// endlessLine is a reader that produces a single SSE line forever and never a
// newline: the exact shape the per-line bound exists for.
type endlessLine struct{ sent int }

func (e *endlessLine) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.sent += len(p)
	if e.sent > 64<<20 {
		// A decoder that respects the bound never gets here. This exists so a
		// broken one fails the test instead of hanging the suite.
		return 0, errors.New("decoder read 64 MiB of a single line without bounding it")
	}
	return len(p), nil
}

func TestSSEDecoderBoundsALineThatNeverEnds(t *testing.T) {
	r := &endlessLine{}
	dec := httpcore.NewSSEDecoder(r, httpcore.SSEConfig{MaxLine: 4096})

	if _, err := dec.Next(); !errors.Is(err, httpcore.ErrSSELineTooLong) {
		t.Fatalf("Next() err = %v, want ErrSSELineTooLong", err)
	}
	// The point of the bound is the memory ceiling, not merely the error: the
	// decoder must have stopped reading near MaxLine rather than at some much
	// larger internal limit. Two doublings of headroom is generous.
	if r.sent > 4*4096 {
		t.Fatalf("decoder read %d bytes of an unterminated 4096-byte-bounded line", r.sent)
	}
}

func TestSSEDecoderRejectsOverSizedFrame(t *testing.T) {
	// Every line here is tiny, so the per-line bound never fires. Only the
	// per-frame total catches this, which is the whole reason it exists: the
	// server sends data: forever and never the blank line that would dispatch.
	var b strings.Builder
	b.WriteString("event: update\n")
	for range 100 {
		b.WriteString("data: " + strings.Repeat("y", 10) + "\n")
	}
	b.WriteString("\n")

	dec := httpcore.NewSSEDecoder(strings.NewReader(b.String()), httpcore.SSEConfig{MaxLine: 4096, MaxFrame: 100})
	if _, err := dec.Next(); !errors.Is(err, httpcore.ErrSSEFrameTooLong) {
		t.Fatalf("Next() err = %v, want ErrSSEFrameTooLong", err)
	}
}

func TestSSEDecoderAcceptsFrameExactlyAtTheBound(t *testing.T) {
	// A bound that rejects at the limit rather than past it would silently
	// refuse frames a backend is entitled to send. "aaaaa\nbbbbb" is 11 bytes
	// including the joining newline the bound counts.
	payload := "data: aaaaa\ndata: bbbbb\n\n"
	dec := httpcore.NewSSEDecoder(strings.NewReader(payload), httpcore.SSEConfig{MaxFrame: 11})

	ev, err := dec.Next()
	if err != nil {
		t.Fatalf("Next() err = %v, want a frame", err)
	}
	if got, want := string(ev.Data), "aaaaa\nbbbbb"; got != want {
		t.Fatalf("Data = %q, want %q", got, want)
	}
}

func TestSSEBoundErrorsAreRetryableNotTerminal(t *testing.T) {
	// mamori's fieldUnhealthy treats KindInvalid as terminal. A hostile frame
	// must not mark a field permanently unhealthy: the stream is torn down and
	// reconnected, so the honest kind is the retryable one.
	for _, err := range []error{httpcore.ErrSSELineTooLong, httpcore.ErrSSEFrameTooLong} {
		if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
			t.Errorf("ErrorKind(%v) = %v, want KindUnavailable", err, got)
		}
	}
}

func TestSSEDecoderFrameDataIsNotReused(t *testing.T) {
	// Both consumers keep a frame's bytes while they decode JSON out of it. A
	// decoder recycling one buffer would corrupt the previous frame in place.
	payload := "data: first\n\ndata: second\n\n"
	dec := httpcore.NewSSEDecoder(strings.NewReader(payload), httpcore.SSEConfig{})

	first, err := dec.Next()
	if err != nil {
		t.Fatalf("first Next() err = %v", err)
	}
	if _, err := dec.Next(); err != nil {
		t.Fatalf("second Next() err = %v", err)
	}
	if got := string(first.Data); got != "first" {
		t.Fatalf("first frame's Data changed to %q after the next frame was read", got)
	}
}

func TestSSEDecoderReportsReaderErrorVerbatim(t *testing.T) {
	// A transport failure must reach the caller intact so a provider can
	// classify it; this package must not flatten it into an opaque error.
	sentinel := errors.New("transport exploded")
	dec := httpcore.NewSSEDecoder(io.MultiReader(strings.NewReader("data: x\n"), errReader{sentinel}), httpcore.SSEConfig{})

	if _, err := dec.Next(); !errors.Is(err, sentinel) {
		t.Fatalf("Next() err = %v, want the reader's own error", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// --- SSEStream, the net/http binding ---

// streamServer serves one frame and then holds the connection open until the
// client goes away, which is what a real SSE backend does between changes.
func streamServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: put\ndata: hello\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts
}

func openStream(t *testing.T, ctx context.Context, url string, cfg httpcore.SSEConfig) *httpcore.SSEStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	return httpcore.NewSSEStream(ctx, resp, cfg)
}

// openDetachedStream opens a stream whose REQUEST is not bound to ctx, only the
// stream is. That separation is the point: when the request carries the same
// context, net/http aborts the round trip on cancellation all by itself and a
// stream that did nothing at all would look like it worked. NewSSEStream takes
// ctx and resp as separate arguments precisely because a provider may have
// obtained the response through a client of its own, so this is the shape that
// actually exercises what this type promises.
func openDetachedStream(t *testing.T, ctx context.Context, url string, cfg httpcore.SSEConfig) *httpcore.SSEStream {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	return httpcore.NewSSEStream(ctx, resp, cfg)
}

func TestSSEStreamCancelEndsADetachedStream(t *testing.T) {
	ts := streamServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := openDetachedStream(t, ctx, ts.URL, httpcore.SSEConfig{})
	defer func() { _ = s.Close() }()

	if _, err := s.Next(); err != nil {
		t.Fatalf("first Next() err = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		// Nothing but this stream's own body close can unblock that read, and
		// nothing but its own error translation can turn the resulting
		// "use of closed network connection" back into a cancellation.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next() after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Next() did not return within 3s of the context being cancelled")
	}
}

// countingBody is a response body that records how many times it is closed and
// blocks reads until it is.
type countingBody struct {
	mu     sync.Mutex
	closes int
	closed chan struct{}
	once   sync.Once
}

func (c *countingBody) Read([]byte) (int, error) {
	<-c.closed
	return 0, errors.New("read on a closed body")
}

func (c *countingBody) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *countingBody) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func TestSSEStreamClosesTheBodyExactlyOnce(t *testing.T) {
	// A body may be anything a provider's transport returns, not necessarily
	// net/http's own (which happens to tolerate a double close). Both the
	// context callback and an explicit Close race for the same body, and this
	// package must resolve that to exactly one close rather than relying on
	// whatever the body underneath happens to forgive.
	body := &countingBody{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := httpcore.NewSSEStream(ctx, &http.Response{StatusCode: http.StatusOK, Body: body}, httpcore.SSEConfig{})

	cancel()
	select {
	case <-body.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the context callback never closed the body")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close() err = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() err = %v", err)
	}
	if got := body.count(); got != 1 {
		t.Fatalf("body closed %d times, want exactly 1", got)
	}
}

func TestSSEStreamBoundsApplyOverHTTP(t *testing.T) {
	// The bounds are not merely a property of the decoder in isolation; they
	// must survive the net/http binding, which is the only way a real backend
	// reaches them.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+strings.Repeat("z", 4096)+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStream(t, ctx, ts.URL, httpcore.SSEConfig{MaxLine: 64})
	defer func() { _ = s.Close() }()

	if _, err := s.Next(); !errors.Is(err, httpcore.ErrSSELineTooLong) {
		t.Fatalf("Next() err = %v, want ErrSSELineTooLong", err)
	}
}

func TestSSEStreamLeavesNoGoroutineBehind(t *testing.T) {
	// httpcore takes no third-party dependency, so goleak is not available
	// here; the providers' conformance kits run goleak.VerifyNone over the
	// migrated watches. This is the module-local equivalent: open and end a
	// stream both ways (context cancellation and an explicit Close) many times
	// and require the goroutine count to come back to where it started.
	ts := streamServer(t)
	settle(t)
	before := runtime.NumGoroutine()

	for i := range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		s := openStream(t, ctx, ts.URL, httpcore.SSEConfig{})
		if _, err := s.Next(); err != nil {
			t.Fatalf("iteration %d: Next() err = %v", i, err)
		}
		if i%2 == 0 {
			// Ended by cancellation: the context callback runs and must exit.
			cancel()
		} else {
			// Ended by the caller: the callback must never start at all.
			_ = s.Close()
			cancel()
		}
		_ = s.Close()
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d across 50 streams", before, after)
	}
}

// settle waits for transient goroutines (http transport readers, context
// callbacks) to finish, so a goroutine count means what it says. It returns as
// soon as the count holds steady rather than sleeping a fixed budget.
func settle(t *testing.T) {
	t.Helper()
	last, stable := -1, 0
	for range 400 {
		n := runtime.NumGoroutine()
		if n == last {
			if stable++; stable >= 5 {
				return
			}
		} else {
			last, stable = n, 0
		}
		time.Sleep(5 * time.Millisecond)
	}
}
