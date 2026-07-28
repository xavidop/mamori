package mamori

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// blockUntilTimersBudget bounds blockUntilTimers. It is a safety net, not a
// timing assumption: in a passing run the wait is satisfied as soon as the
// watched goroutine arms its timer, usually in microseconds. It only matters
// when the timer is never armed at all, where a bounded failure with a clear
// message beats hanging until the package test timeout.
const blockUntilTimersBudget = 5 * time.Second

// blockUntilTimers waits for n timers to be armed on clk before the caller
// advances it, failing the test if they never are. Every clk.Advance that is
// meant to fire a timer belonging to another goroutine needs this first; see
// FakeClock.BlockUntil for the race it closes.
func blockUntilTimers(t *testing.T, clk *FakeClock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), blockUntilTimersBudget)
	defer cancel()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d pending timer(s) on the fake clock: %v", n, err)
	}
}

// armedTimers reports how many timers are currently pending on clk. It is the
// counting half of armedDelay, used to assert that a code path arms no timer
// at all (a natively-watched ref, which never enters the polling adapter).
func armedTimers(clk *FakeClock) int {
	clk.mu.Lock()
	defer clk.mu.Unlock()
	return len(clk.waiters)
}

// armedDelay returns the delay of the single timer currently pending on clk,
// measured from the clock's current time.
//
// It is the assertion counterpart to blockUntilTimers: the block orders the
// test after the goroutine armed its timer, and this reads the deadline that
// goroutine chose. Reading the deadline directly is what makes a retry-cadence
// assertion exact - the alternative is to advance the clock by a guessed amount
// and infer the interval from whether a side effect happened, which can only
// bracket the delay and turns every cadence test into a timing puzzle. It
// insists on exactly one pending timer so a test that has accidentally armed a
// debounce timer alongside the poll timer fails loudly rather than asserting on
// whichever one happens to be first in the slice.
func armedDelay(t *testing.T, clk *FakeClock) time.Duration {
	t.Helper()
	clk.mu.Lock()
	defer clk.mu.Unlock()
	if len(clk.waiters) != 1 {
		t.Fatalf("pending timers on the fake clock = %d, want exactly 1", len(clk.waiters))
	}
	return clk.waiters[0].deadline.Sub(clk.now)
}

func TestFakeClockTimer(t *testing.T) {
	c := NewFakeClock(time.Time{})
	timer := c.NewTimer(10 * time.Second)
	select {
	case <-timer.C:
		t.Fatal("timer fired early")
	default:
	}
	c.Advance(5 * time.Second)
	select {
	case <-timer.C:
		t.Fatal("timer fired at half the interval")
	default:
	}
	c.Advance(5 * time.Second)
	select {
	case <-timer.C:
	case <-time.After(time.Second):
		t.Fatal("timer did not fire after full interval")
	}
}

func TestFakeClockTicker(t *testing.T) {
	c := NewFakeClock(time.Time{})
	tk := c.NewTicker(time.Second)
	defer tk.Stop()
	count := 0
	c.Advance(3 * time.Second)
	// drain: fake ticker has buffered channel of 1 but Advance fires all; count
	// the ticks available.
	for {
		select {
		case <-tk.C:
			count++
			continue
		default:
		}
		break
	}
	if count == 0 {
		t.Fatal("ticker did not tick after advancing past interval")
	}
}

func TestFakeClockAfter(t *testing.T) {
	c := NewFakeClock(time.Time{})
	ch := c.After(time.Minute)
	c.Advance(time.Minute)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("After channel did not unblock on advance")
	}
}

func TestFakeClockBlockUntilAlreadySatisfied(t *testing.T) {
	c := NewFakeClock(time.Time{})
	timer := c.NewTimer(time.Second)
	defer timer.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("BlockUntil with the timer already armed = %v, want nil", err)
	}
	// n == 0 is trivially true even with nothing armed.
	if err := c.BlockUntil(ctx, 0); err != nil {
		t.Fatalf("BlockUntil(0) = %v, want nil", err)
	}
}

// TestFakeClockBlockUntilOrdersAdvance is the regression test for the race
// BlockUntil exists to close: a goroutine that arms its timer only after the
// test has started running must still have that timer fired by Advance.
func TestFakeClockBlockUntilOrdersAdvance(t *testing.T) {
	defer goleak.VerifyNone(t)

	c := NewFakeClock(time.Time{})
	fired := make(chan time.Time, 1)
	start := make(chan struct{})
	go func() {
		<-start
		// Deliberately arm late, the way a real watched goroutine does: after
		// the test goroutine is already on its way to Advance.
		fired <- <-c.NewTimer(10 * time.Second).C
	}()
	close(start)

	blockUntilTimers(t, c, 1)
	c.Advance(10 * time.Second)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timer armed after BlockUntil returned did not fire on Advance")
	}
}

func TestFakeClockBlockUntilContextExpires(t *testing.T) {
	defer goleak.VerifyNone(t)

	c := NewFakeClock(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.BlockUntil(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BlockUntil with nothing ever armed = %v, want context.DeadlineExceeded", err)
	}
	// The abandoned blocker must not be left behind to be closed twice by a
	// later registration.
	timer := c.NewTimer(time.Second)
	defer timer.Stop()
	timer2 := c.NewTimer(time.Second)
	defer timer2.Stop()
}

// TestFakeClockBlockUntilCountsPendingOnly pins the two properties BlockUntil's
// doc comment calls out: an immediately-firing timer never becomes pending, and
// the count drops again once a timer fires.
func TestFakeClockBlockUntilCountsPendingOnly(t *testing.T) {
	c := NewFakeClock(time.Time{})
	c.NewTimer(0) // fires immediately, never pending

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.BlockUntil(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NewTimer(0) counted as pending: BlockUntil = %v", err)
	}

	timer := c.NewTimer(time.Second)
	blockUntilTimers(t, c, 1)
	c.Advance(time.Second)
	<-timer.C

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := c.BlockUntil(ctx2, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fired timer still counted as pending: BlockUntil = %v", err)
	}
}

func TestFakeClockNow(t *testing.T) {
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	c.Advance(90 * time.Minute)
	if got := c.Now(); !got.Equal(start.Add(90 * time.Minute)) {
		t.Fatalf("Now = %v, want %v", got, start.Add(90*time.Minute))
	}
}
