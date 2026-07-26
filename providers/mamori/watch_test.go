package mamoriprov

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"go.uber.org/goleak"
)

// recvUpdate waits for one mamori.Update on ch, failing the test if the
// channel closes or nothing arrives within a generous deadline.
func recvUpdate(t *testing.T, ch <-chan mamori.Update) mamori.Update {
	t.Helper()
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly while waiting for an update")
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an update")
	}
	return mamori.Update{}
}

func TestWatchDeliversUpdate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"aHVudGVyMg==","version":"v1","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done() // keep the connection open until the client disconnects
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	u := recvUpdate(t, ch)
	if u.Err != nil {
		t.Fatalf("Update.Err = %v, want nil", u.Err)
	}
	if got, want := string(u.Value.Bytes), "hunter2"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if got, want := u.Value.Version, "v1"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
}

func TestWatchDeliversErrorFrameKeepsChannelOpen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"name":"db-password","error":{"kind":"permission_denied","message":"denied"}}`)
		flusher.Flush()
		fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"aGVsbG8=","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	u1 := recvUpdate(t, ch)
	if !errors.Is(u1.Err, mamori.ErrPermissionDenied) {
		t.Fatalf("first Update.Err = %v, want errors.Is(..., mamori.ErrPermissionDenied)", u1.Err)
	}

	u2 := recvUpdate(t, ch)
	if u2.Err != nil {
		t.Fatalf("second Update.Err = %v, want nil (channel must stay open after a transient error)", u2.Err)
	}
	if got, want := string(u2.Value.Bytes), "hello"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
}

func TestWatchReconnectsAfterDisconnect(t *testing.T) {
	var reqCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		if n == 1 {
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"Zmlyc3Q=","metadata":{}}`)
			flusher.Flush()
			return // close the connection after the first frame; the client must reconnect
		}

		fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"c2Vjb25k","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	u1 := recvUpdate(t, ch)
	if got, want := string(u1.Value.Bytes), "first"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}

	u2 := recvUpdate(t, ch)
	if got, want := string(u2.Value.Bytes), "second"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}

	if got := atomic.LoadInt32(&reqCount); got < 2 {
		t.Fatalf("request count = %d, want >= 2 (a reconnect must have happened)", got)
	}
}

func TestWatchClosesChannelOnContextCancel(t *testing.T) {
	// goleak.VerifyNone is deferred first so it runs LAST (defers are LIFO):
	// ts.Close below must complete - shutting down httptest's own
	// accept-loop goroutine - before the leak check runs, or that goroutine
	// (not the provider's) would be misreported as a leak.
	defer goleak.VerifyNone(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // held open; only the client's ctx cancellation ends it
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed, as required
			}
		case <-deadline:
			t.Fatal("watch channel not closed after context cancellation")
		}
	}
}

func TestWatchIgnoresHeartbeatComments(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, ": heartbeat\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"aGVsbG8=","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	u := recvUpdate(t, ch)
	if u.Err != nil {
		t.Fatalf("Update.Err = %v, want nil", u.Err)
	}
	if got, want := string(u.Value.Bytes), "hello"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}

	// The heartbeat comment must not have produced a second, spurious Update.
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra Update after a heartbeat comment: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestWatchRejectsOversizedFrame guards against unbounded per-frame memory
// growth: a server that streams data: lines past sseMaxFrameBytes without
// ever sending the blank line that would dispatch the frame must not be
// allowed to make the client wait forever (or grow memory forever) reading
// that one frame. The first connection does exactly that - and, crucially,
// never closes on its own (it blocks on r.Context().Done()) - so the only
// way the client ever reconnects is if readSSE itself gives up once the
// accumulated frame crosses the cap. Against the pre-fix, per-line-only
// bound, Scan() would simply block waiting for more bytes the server never
// sends, no reconnect would happen, and this test would time out. Against
// the fix, readSSE returns as soon as the running total crosses
// sseMaxFrameBytes, the connection tears down, and watchLoop reconnects
// quickly (backoff resets to the floor since the first connection did
// establish) onto a second, well-formed frame.
func TestWatchRejectsOversizedFrame(t *testing.T) {
	var reqCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		if n == 1 {
			// Stream well past sseMaxFrameBytes of data: lines and never
			// send the blank line that would dispatch the frame, then hold
			// the connection open indefinitely. A well-behaved client must
			// give up on this frame on its own instead of waiting forever
			// for a blank line that is never coming.
			fmt.Fprintf(w, "event: update\n")
			chunk := strings.Repeat("a", 64*1024) // 64 KiB per data: line
			for sent := 0; sent <= sseMaxFrameBytes; sent += len(chunk) {
				fmt.Fprintf(w, "data: %s\n", chunk)
				flusher.Flush()
			}
			<-r.Context().Done() // no blank line ever follows
			return
		}

		// Second connection (the reconnect): a normal, small, well-formed
		// frame.
		fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"c2Vjb25k","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-ch:
			if u.Err == nil && len(u.Value.Bytes) > sseMaxFrameBytes {
				t.Fatalf("delivered an oversized Update (%d bytes); an over-cap frame must be dropped, not delivered", len(u.Value.Bytes))
			}
			if u.Err == nil && string(u.Value.Bytes) == "second" {
				// The well-formed frame from the reconnect arrived: the
				// oversized first frame was torn down, not hung on or
				// delivered.
				if got := atomic.LoadInt32(&reqCount); got < 2 {
					t.Fatalf("request count = %d, want >= 2 (a reconnect must have happened)", got)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a reconnect after an oversized frame (request count = %d); the client appears to be hung reading an ever-growing frame instead of tearing the connection down", atomic.LoadInt32(&reqCount))
		}
	}
}

func TestWatchEmptyNameIsInvalid(t *testing.T) {
	p := New(Config{Endpoint: "http://unused", InsecureNoTLS: true})

	ref := mamori.Ref{Scheme: "mamori", Raw: "mamori://"}
	_, err := p.Watch(context.Background(), ref)
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}
