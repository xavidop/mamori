package httpcore

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// recvUpdate takes one Update, failing if none arrives in time or the channel
// closed first.
func recvUpdate(t *testing.T, ch <-chan mamori.Update) mamori.Update {
	t.Helper()
	select {
	case u, open := <-ch:
		if !open {
			t.Fatal("channel closed before an Update arrived")
		}
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("no Update within 3s")
	}
	return mamori.Update{}
}

// awaitClosed fails unless ch closes within 3s. Because LongPoll closes the
// channel from a deferred close in its one goroutine, a closed channel is also
// proof that goroutine has returned.
func awaitClosed(t *testing.T, ch <-chan mamori.Update) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 3s")
		}
	}
}

func TestLongPollRequiresRound(t *testing.T) {
	ch, err := LongPoll(context.Background(), LongPollConfig{})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want mamori.ErrInvalid", err)
	}
	if ch != nil {
		t.Fatal("channel should be nil when the config is rejected")
	}
}

func TestLongPollEmitsBaselineThenChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var rounds int
	ch, err := LongPoll(ctx, LongPollConfig{
		Baseline: func(context.Context) (LongPollResult, error) {
			return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("base")}}, nil
		},
		Round: func(rctx context.Context, _ time.Duration) (LongPollResult, error) {
			rounds++
			if rounds == 1 {
				return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("next")}}, nil
			}
			<-rctx.Done()
			return LongPollResult{}, rctx.Err()
		},
		Hold: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	if got := string(recvUpdate(t, ch).Value.Bytes); got != "base" {
		t.Fatalf("first Update = %q, want base", got)
	}
	if got := string(recvUpdate(t, ch).Value.Bytes); got != "next" {
		t.Fatalf("second Update = %q, want next", got)
	}
}

func TestLongPollUnchangedRoundEmitsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var rounds int
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(rctx context.Context, _ time.Duration) (LongPollResult, error) {
			mu.Lock()
			rounds++
			n := rounds
			mu.Unlock()
			switch {
			case n < 5:
				return LongPollResult{}, nil // the hold elapsed with nothing to report
			case n == 5:
				return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("finally")}}, nil
			default:
				// Park, so the round count is stable when the test reads it.
				<-rctx.Done()
				return LongPollResult{}, rctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	// The first four rounds must produce no Update at all; the fifth must.
	if got := string(recvUpdate(t, ch).Value.Bytes); got != "finally" {
		t.Fatalf("Update = %q, want finally (an unchanged round must emit nothing)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	// Four rounds ran before the one that changed. Had an unchanged round
	// emitted, the Update above would have arrived at round 1 carrying nothing.
	if rounds < 5 {
		t.Fatalf("rounds = %d, want at least 5", rounds)
	}
}

func TestLongPollDeliversErrorAndKeepsWatching(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	boom := errors.New("boom")
	var rounds int
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(context.Context, time.Duration) (LongPollResult, error) {
			rounds++
			if rounds == 1 {
				return LongPollResult{}, boom
			}
			return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("recovered")}}, nil
		},
		ErrorPause: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	if u := recvUpdate(t, ch); !errors.Is(u.Err, boom) {
		t.Fatalf("first Update Err = %v, want boom", u.Err)
	}
	// The channel must NOT have closed: a transient failure ends no watch,
	// because mamori never resubscribes a closed one.
	if got := string(recvUpdate(t, ch).Value.Bytes); got != "recovered" {
		t.Fatalf("second Update = %q, want recovered", got)
	}
}

func TestLongPollErrorPausePacesTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const pause = 40 * time.Millisecond
	var mu sync.Mutex
	var starts []time.Time
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(context.Context, time.Duration) (LongPollResult, error) {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			return LongPollResult{}, errors.New("instant failure")
		},
		ErrorPause: pause,
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	// Drain three failures, then stop the loop.
	for range 3 {
		if u := recvUpdate(t, ch); u.Err == nil {
			t.Fatal("expected an error Update")
		}
	}
	cancel()
	awaitClosed(t, ch)

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 3 {
		t.Fatalf("saw %d rounds, want at least 3", len(starts))
	}
	// Without the pause an instantly-failing backend is hit as fast as the
	// scheduler allows. With it, consecutive rounds are at least a pause apart.
	for i := 1; i < 3; i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < pause {
			t.Fatalf("rounds %d and %d were %v apart, want at least %v: a fast-failing backend must not be hot-looped",
				i-1, i, gap, pause)
		}
	}
}

func TestLongPollRoundDeadlineIsHoldPlusGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const hold = 200 * time.Millisecond
	const grace = 300 * time.Millisecond

	type observed struct {
		hold        time.Duration
		remaining   time.Duration
		hasDeadline bool
	}
	seen := make(chan observed, 1)
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(rctx context.Context, h time.Duration) (LongPollResult, error) {
			dl, ok := rctx.Deadline()
			select {
			case seen <- observed{hold: h, remaining: time.Until(dl), hasDeadline: ok}:
			default:
			}
			<-rctx.Done()
			return LongPollResult{}, rctx.Err()
		},
		Hold:  hold,
		Grace: grace,
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}
	defer func() { cancel(); awaitClosed(t, ch) }()

	var got observed
	select {
	case got = <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("Round was never called")
	}

	if !got.hasDeadline {
		t.Fatal("Round's context carries no deadline; a hung backend would hang the watch forever")
	}
	if got.hold != hold {
		t.Fatalf("hold passed to Round = %v, want %v", got.hold, hold)
	}
	// The client-side budget must be strictly longer than the hold the backend
	// was told to use, or every idle round dies on the client instead of
	// returning "nothing changed".
	if got.remaining <= hold {
		t.Fatalf("Round deadline is %v away but the backend was asked to hold for %v: the client must outlast the server",
			got.remaining, hold)
	}
	if got.remaining > hold+grace {
		t.Fatalf("Round deadline is %v away, want at most hold+grace = %v", got.remaining, hold+grace)
	}
}

func TestLongPollDefaultsAreApplied(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(chan time.Duration, 1)
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(rctx context.Context, h time.Duration) (LongPollResult, error) {
			select {
			case seen <- h:
			default:
			}
			<-rctx.Done()
			return LongPollResult{}, rctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}
	defer func() { cancel(); awaitClosed(t, ch) }()

	select {
	case h := <-seen:
		if h != DefaultLongPollHold {
			t.Fatalf("hold = %v, want DefaultLongPollHold (%v)", h, DefaultLongPollHold)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Round was never called")
	}
}

func TestLongPollBaselineIsBoundedByGraceAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const hold = 10 * time.Second
	const grace = 150 * time.Millisecond

	seen := make(chan time.Duration, 1)
	ch, err := LongPoll(ctx, LongPollConfig{
		Baseline: func(bctx context.Context) (LongPollResult, error) {
			dl, ok := bctx.Deadline()
			if !ok {
				t.Error("Baseline context carries no deadline")
				return LongPollResult{}, nil
			}
			seen <- time.Until(dl)
			return LongPollResult{}, nil
		},
		Round: func(rctx context.Context, _ time.Duration) (LongPollResult, error) {
			<-rctx.Done()
			return LongPollResult{}, rctx.Err()
		},
		Hold:  hold,
		Grace: grace,
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}
	defer func() { cancel(); awaitClosed(t, ch) }()

	select {
	case remaining := <-seen:
		// Grace alone, not Hold+Grace: the baseline is an ordinary request.
		if remaining > grace {
			t.Fatalf("baseline deadline is %v away, want at most Grace (%v)", remaining, grace)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Baseline was never called")
	}
}

func TestLongPollBaselineRunsBeforeTheFirstRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	order := make(chan string, 4)
	ch, err := LongPoll(ctx, LongPollConfig{
		Baseline: func(context.Context) (LongPollResult, error) {
			order <- "baseline"
			return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("b")}}, nil
		},
		Round: func(rctx context.Context, _ time.Duration) (LongPollResult, error) {
			select {
			case order <- "round":
			default:
			}
			<-rctx.Done()
			return LongPollResult{}, rctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}
	defer func() { cancel(); awaitClosed(t, ch) }()

	if got := <-order; got != "baseline" {
		t.Fatalf("first call = %q, want baseline: the subscription state a Round sends is established by Baseline", got)
	}
	if got := <-order; got != "round" {
		t.Fatalf("second call = %q, want round", got)
	}
}

func TestLongPollClosesOnCancelAndEmitsNoCancellationError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	var once sync.Once
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(rctx context.Context, _ time.Duration) (LongPollResult, error) {
			once.Do(func() { close(started) })
			<-rctx.Done()
			// Exactly what a real HTTP round trip returns once the context
			// dies. It must not reach the caller as a watch failure.
			return LongPollResult{}, rctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	<-started
	cancel()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				return // closed, and nothing spurious was delivered
			}
			t.Fatalf("received %+v after cancellation; a cancelled watch must close silently", u)
		case <-deadline:
			t.Fatal("channel not closed within 3s after cancellation")
		}
	}
}

func TestLongPollStopsWhenNobodyReceives(t *testing.T) {
	// A consumer that stops receiving must not pin the loop goroutine past
	// cancellation: emit selects on ctx.Done alongside the send.
	//
	// This has to be asserted WITHOUT receiving, and that is the whole
	// difficulty. Draining the channel unblocks a parked send by itself, so a
	// test that waits for closure passes whether or not emit has its ctx arm:
	// the value is taken, the loop resumes, sees the dead context, and closes.
	// The only observer that does not disturb what it measures is the goroutine
	// count.
	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := LongPoll(ctx, LongPollConfig{
		Round: func(context.Context, time.Duration) (LongPollResult, error) {
			return LongPollResult{Changed: true, Value: mamori.Value{Bytes: []byte("x")}}, nil
		},
	})
	if err != nil {
		t.Fatalf("LongPoll: %v", err)
	}

	// Let the loop fill the one-slot buffer and park on the next send.
	time.Sleep(50 * time.Millisecond)
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			// Gone. Now draining is safe and must find a closed channel.
			awaitClosed(t, ch)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the loop goroutine is still running 3s after cancellation (%d goroutines, was %d before Watch): "+
		"a consumer that stopped receiving has pinned it on a send", runtime.NumGoroutine(), before)
}
