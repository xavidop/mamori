package mamori_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// This file covers Task 1 of the config-server plan (WatchRef): the exported
// WatchRef must make exactly the same native-watch-or-poll choice
// engine.start makes for every ref in a Watch[T] chain, so a caller that
// wants to watch a single ref outside of Watch[T] - the config server, most
// notably - gets identical behavior rather than a second implementation of
// the same decision.

// wrefStaticProvider is a non-watchable mamori.Provider (it does not
// implement mamori.WatchableProvider), so WatchRef has no choice but to fall
// back to the polling adapter for it. It mirrors the pollFake fixture
// poll_test.go uses to exercise pollWatch directly, kept as its own type
// here since that one is unexported to the internal test package.
type wrefStaticProvider struct {
	scheme string
	mu     sync.Mutex
	val    mamori.Value
}

func (p *wrefStaticProvider) Scheme() string { return p.scheme }

func (p *wrefStaticProvider) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.val, nil
}

func (p *wrefStaticProvider) setVal(v mamori.Value) {
	p.mu.Lock()
	p.val = v
	p.mu.Unlock()
}

func wrefDrainOne(t *testing.T, ch <-chan mamori.Update, wantBytes string) {
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

func wrefNoUpdate(t *testing.T, ch <-chan mamori.Update) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("unexpected update: %+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestWatchRefUsesNativeWatch proves WatchRef picks a WatchableProvider's own
// Watch (rather than polling it) exactly as engine.start's per-position loop
// does: mamoritest.Provider delivers the current value as an immediate
// baseline and then a further Update on every Set, with no poll interval
// elapsing in between (this test drives no clock at all, real or fake -
// if WatchRef were polling instead, the Set below would only be observed
// after a real pollInterval tick, which this test does not wait for).
func TestWatchRefUsesNativeWatch(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("wref-native")
	p.Set("cfg/level", "info")
	ref := mamori.Ref{Scheme: "wref-native", Path: "cfg/level"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := mamori.WatchRef(ctx, p, ref)

	wrefDrainOne(t, ch, "info") // baseline, replayed by mamoritest.Provider.Watch
	p.Set("cfg/level", "debug")
	wrefDrainOne(t, ch, "debug") // pushed natively, not polled

	cancel()
	for range ch { // drain to closure, proving no goroutine is left running
	}
}

// TestWatchRefPollsNonWatchableProvider proves WatchRef falls back to the
// polling adapter for a Provider that does not implement WatchableProvider,
// exactly as engine.start's per-position loop does: a value change is only
// observed once a poll tick (driven here by FakeClock, deterministically)
// elapses, not immediately as the native-watch test above requires.
func TestWatchRefPollsNonWatchableProvider(t *testing.T) {
	defer goleak.VerifyNone(t)

	clk := mamori.NewFakeClock(time.Time{})
	p := &wrefStaticProvider{scheme: "wref-poll", val: mamori.Value{Bytes: []byte("v1"), Version: "1"}}
	ref := mamori.Ref{Scheme: "wref-poll", Path: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	ch := mamori.WatchRef(ctx, p, ref,
		mamori.WithClock(clk), mamori.WithJitter(0), mamori.WithPollInterval(10*time.Second))

	wrefDrainOne(t, ch, "v1") // initial baseline, exactly as pollWatch emits

	// Unchanged across a tick -> no update.
	clk.Advance(11 * time.Second)
	wrefNoUpdate(t, ch)

	// Changed value -> one update, only after the poll interval elapses.
	p.setVal(mamori.Value{Bytes: []byte("v2"), Version: "2"})
	clk.Advance(11 * time.Second)
	wrefDrainOne(t, ch, "v2")

	cancel()
	for range ch { // drain to closure
	}
}
