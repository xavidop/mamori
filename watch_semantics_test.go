package mamori

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

type coalesceConfig struct {
	A string `source:"w://a"`
	B string `source:"w://b"`
	C string `source:"w://c"`
}

func TestWatchCoalescing(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("a", "1", "a1")
	wp.set("b", "1", "b1")
	wp.set("c", "1", "c1")

	events := make(chan Change[coalesceConfig], 4)
	w, err := Watch[coalesceConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(ev Change[coalesceConfig]) { events <- ev }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Three field changes within one debounce window.
	wp.push("a", "2", "a2")
	wp.push("b", "2", "b2")
	wp.push("c", "2", "c2")
	waitPending(clk)
	clk.Advance(defaultDebounce)

	select {
	case ev := <-events:
		if len(ev.Fields) != 3 {
			t.Fatalf("coalesced event has %d fields, want 3: %+v", len(ev.Fields), ev.Fields)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no coalesced event")
	}
	// No second event.
	select {
	case ev := <-events:
		t.Fatalf("unexpected second event: %+v", ev.Fields)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchSerializedDispatch(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var count atomic.Int32
	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(Change[watchConfig]) {
			c := concurrent.Add(1)
			if c > maxConcurrent.Load() {
				maxConcurrent.Store(c)
			}
			time.Sleep(5 * time.Millisecond)
			concurrent.Add(-1)
			count.Add(1)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < 5; i++ {
		ver := "v" + string(rune('a'+i))
		wp.push("prod/db#password", "p"+ver, ver)
		waitPending(clk)
		clk.Advance(defaultDebounce)
	}
	// Wait for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for count.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if maxConcurrent.Load() > 1 {
		t.Fatalf("OnChange ran concurrently (max=%d), must be serialized", maxConcurrent.Load())
	}
}

func TestWatchDropOldest(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	gate := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got []string

	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk), WithQueueDepth(1),
		OnChange(func(ev Change[watchConfig]) {
			once.Do(func() { <-gate }) // block the first delivery
			mu.Lock()
			got = append(got, ev.New.Password.Reveal())
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for i := 2; i <= 5; i++ {
		ver := "v" + string(rune('0'+i))
		wp.push("prod/db#password", "p"+ver, ver)
		waitPending(clk)
		clk.Advance(defaultDebounce)
		time.Sleep(20 * time.Millisecond)
	}
	close(gate) // release the blocked first delivery

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		last := ""
		if n > 0 {
			last = got[n-1]
		}
		mu.Unlock()
		if last == "pv5" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) >= 4 {
		t.Fatalf("expected some events dropped with queue depth 1, got all %d: %v", len(got), got)
	}
	if got[len(got)-1] != "pv5" {
		t.Fatalf("last delivered = %q, want pv5 (final value must win)", got[len(got)-1])
	}
	if w.Get().Password.Reveal() != "pv5" {
		t.Fatalf("Get() = %q, want pv5", w.Get().Password.Reveal())
	}
}

// errAfterProvider succeeds on the first N resolves then returns an error, to
// exercise the runtime failure path. It is non-watchable (poll-driven).
type errAfterProvider struct {
	scheme string
	val    Value
	ok     atomic.Int32 // remaining successful resolves
	fail   error
}

func (p *errAfterProvider) Scheme() string { return p.scheme }
func (p *errAfterProvider) Resolve(context.Context, Ref) (Value, error) {
	if p.ok.Add(-1) >= 0 {
		return p.val, nil
	}
	return Value{}, p.fail
}

type backoffConfig struct {
	V string `source:"e://x" default:"init"`
}

func TestWatchBackoffRetainsLastGood(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	p := &errAfterProvider{scheme: "e", val: Value{Bytes: []byte("good"), Version: "1"}, fail: errors.New("boom")}
	p.ok.Store(1) // first resolve (initial load) succeeds, then errors

	errs := make(chan error, 4)
	w, err := Watch[backoffConfig](context.Background(),
		WithProvider(p), WithClock(clk), WithJitter(0),
		WithPollInterval(10*time.Second),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	if w.Get().V != "good" {
		t.Fatalf("initial V = %q, want good", w.Get().V)
	}
	// Trigger a poll that fails.
	time.Sleep(20 * time.Millisecond)
	clk.Advance(11 * time.Second)

	select {
	case e := <-errs:
		var pe *ProviderError
		if !errors.As(e, &pe) {
			t.Fatalf("error = %T, want *ProviderError", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnError did not fire on resolve failure")
	}
	if w.Get().V != "good" {
		t.Fatalf("after error V = %q, want good (last-good retained)", w.Get().V)
	}
}

func TestWatchShutdownNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(Change[watchConfig]) {}),
	)
	if err != nil {
		t.Fatal(err)
	}
	wp.push("prod/db#password", "new", "v2")
	waitPending(clk)
	clk.Advance(defaultDebounce)
	time.Sleep(30 * time.Millisecond)

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
