package mamori

import (
	"testing"
	"time"
)

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

func TestFakeClockNow(t *testing.T) {
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	c.Advance(90 * time.Minute)
	if got := c.Now(); !got.Equal(start.Add(90 * time.Minute)) {
		t.Fatalf("Now = %v, want %v", got, start.Add(90*time.Minute))
	}
}
