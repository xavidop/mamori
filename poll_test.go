package mamori

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// pollFake is a non-watchable provider whose value can be changed between polls.
type pollFake struct {
	scheme string
	mu     sync.Mutex
	val    Value
	err    error
	calls  atomic.Int32
}

func (p *pollFake) Scheme() string { return p.scheme }
func (p *pollFake) Resolve(context.Context, Ref) (Value, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.val, p.err
}
func (p *pollFake) setVal(v Value) { p.mu.Lock(); p.val = v; p.mu.Unlock() }

func drainOne(t *testing.T, ch <-chan Update, wantBytes string) {
	t.Helper()
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("unexpected update error: %v", u.Err)
		}
		if string(u.Value.Bytes) != wantBytes {
			t.Fatalf("update bytes = %q, want %q", u.Value.Bytes, wantBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no update (wanted %q)", wantBytes)
	}
}

func noUpdate(t *testing.T, ch <-chan Update) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("unexpected update: %+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPollInitialThenChange(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "p", val: Value{Bytes: []byte("v1"), Version: "1"}}
	o := defaultOptions()
	o.clock = clk
	o.jitter = 0
	o.pollInterval = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, Ref{Scheme: "p", Path: "x"}, o)

	drainOne(t, ch, "v1") // initial baseline

	// Unchanged across a tick -> no update.
	clk.Advance(11 * time.Second)
	noUpdate(t, ch)

	// Changed value -> one update.
	p.setVal(Value{Bytes: []byte("v2"), Version: "2"})
	clk.Advance(11 * time.Second)
	drainOne(t, ch, "v2")

	cancel()
	// channel closes on ctx cancel
	select {
	case _, open := <-ch:
		if open {
			// may deliver a final buffered item; drain until closed
			for range ch {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestPollNotAfterEarlyRefresh(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	start := clk.Now()
	p := &pollFake{scheme: "p", val: Value{
		Bytes: []byte("lease1"), Version: "1", NotAfter: start.Add(2 * time.Second),
	}}
	o := defaultOptions()
	o.clock = clk
	o.jitter = 0
	o.pollInterval = 1 * time.Hour // far longer than the lease

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := pollWatch(ctx, p, Ref{Scheme: "p", Path: "x"}, o)
	drainOne(t, ch, "lease1")

	// Change the value; a refresh should happen before the lease NotAfter,
	// well before the 1h poll interval.
	p.setVal(Value{Bytes: []byte("lease2"), Version: "2", NotAfter: start.Add(4 * time.Second)})
	clk.Advance(2 * time.Second)
	drainOne(t, ch, "lease2")
}

func TestPollCtxCancelCloses(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	p := &pollFake{scheme: "p", val: Value{Bytes: []byte("v1"), Version: "1"}}
	o := defaultOptions()
	o.clock = clk
	o.jitter = 0

	ctx, cancel := context.WithCancel(context.Background())
	ch := pollWatch(ctx, p, Ref{Scheme: "p", Path: "x"}, o)
	drainOne(t, ch, "v1")
	cancel()
	for range ch { // drain to closure
	}
}
