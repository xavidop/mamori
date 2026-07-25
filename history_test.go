package mamori_test

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// TestHistoryDefaultRetainsOnlyCurrent verifies that with no WithHistory,
// History returns exactly one snapshot after the initial load: the current
// one, at version 1.
func TestHistoryDefaultRetainsOnlyCurrent(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-history-default")
	p.Set("cfg/level", "info")

	type config struct {
		Level string `source:"mt-history-default://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	hist := w.History()
	if len(hist) != 1 {
		t.Fatalf("History() len = %d, want 1", len(hist))
	}
	if hist[0].Version != 1 {
		t.Fatalf("History()[0].Version = %d, want 1", hist[0].Version)
	}
	if hist[0].Config.Level != "info" {
		t.Fatalf("History()[0].Config.Level = %q, want info", hist[0].Config.Level)
	}
	if len(hist[0].Fields) != 0 {
		t.Fatalf("History()[0].Fields = %+v, want empty for the initial snapshot", hist[0].Fields)
	}
}

// TestHistoryWithHistoryZeroStillOneAfterChange drives one applied change and
// confirms WithHistory(0) (the explicit default) still retains only the
// current snapshot afterward, now at version 2.
func TestHistoryWithHistoryZeroStillOneAfterChange(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-history-zero")
	p.Set("cfg/level", "info")

	type config struct {
		Level string `source:"mt-history-zero://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("cfg/level", "debug")
	mamoritest.WaitForSnapshot(t, w, 2)

	hist := w.History()
	if len(hist) != 1 {
		t.Fatalf("History() len = %d, want 1", len(hist))
	}
	if hist[0].Version != 2 {
		t.Fatalf("History()[0].Version = %d, want 2", hist[0].Version)
	}
	if hist[0].Config.Level != "debug" {
		t.Fatalf("History()[0].Config.Level = %q, want debug", hist[0].Config.Level)
	}
}

// TestHistoryWithHistoryThreeRetainsPriorNewestFirst drives one applied
// change with WithHistory(3) and confirms History returns the current plus
// the prior snapshot, newest first, with descending versions.
func TestHistoryWithHistoryThreeRetainsPriorNewestFirst(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-history-three")
	p.Set("cfg/level", "info")

	type config struct {
		Level string `source:"mt-history-three://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(3))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("cfg/level", "debug")
	mamoritest.WaitForSnapshot(t, w, 2)

	hist := w.History()
	if len(hist) != 2 {
		t.Fatalf("History() len = %d, want 2", len(hist))
	}
	if hist[0].Version != 2 || hist[1].Version != 1 {
		t.Fatalf("History() versions = [%d, %d], want [2, 1] (newest first, descending)",
			hist[0].Version, hist[1].Version)
	}
	if hist[0].Config.Level != "debug" {
		t.Fatalf("newest snapshot Config.Level = %q, want debug", hist[0].Config.Level)
	}
	if hist[1].Config.Level != "info" {
		t.Fatalf("prior snapshot Config.Level = %q, want info", hist[1].Config.Level)
	}

	// The newest snapshot's Fields holds the diff that produced it.
	if len(hist[0].Fields) != 1 {
		t.Fatalf("hist[0].Fields = %+v, want exactly one changed field", hist[0].Fields)
	}
	if hist[0].Fields[0].Path != "Level" {
		t.Fatalf("hist[0].Fields[0].Path = %q, want Level", hist[0].Fields[0].Path)
	}
	// The initial (oldest retained) snapshot carries no diff: nothing preceded it.
	if len(hist[1].Fields) != 0 {
		t.Fatalf("hist[1].Fields = %+v, want empty for the initial snapshot", hist[1].Fields)
	}
}

// TestHistoryBoundedByWithHistory verifies History is bounded: with
// WithHistory(2), after several changes, len(History()) never exceeds 3 (2
// prior + current), and the retained set is always newest-first.
func TestHistoryBoundedByWithHistory(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("mt-history-bounded")
	p.Set("cfg/level", "v0")

	type config struct {
		Level string `source:"mt-history-bounded://cfg/level"`
	}

	w, err := mamori.Watch[config](context.Background(),
		mamori.WithProvider(p), mamori.WithDebounce(0), mamori.WithHistory(2))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	const changes = 6
	for i := 1; i <= changes; i++ {
		p.Set("cfg/level", "v"+string(rune('1'+i-1)))
		mamoritest.WaitForSnapshot(t, w, uint64(i+1))

		hist := w.History()
		if len(hist) > 3 {
			t.Fatalf("after %d changes, History() len = %d, want <= 3", i, len(hist))
		}
		for j := 1; j < len(hist); j++ {
			if hist[j-1].Version <= hist[j].Version {
				t.Fatalf("History() not newest-first: versions %v", versionsOf(hist))
			}
		}
	}

	final := w.History()
	if len(final) != 3 {
		t.Fatalf("final History() len = %d, want 3 (2 prior + current)", len(final))
	}
	wantVersions := []uint64{changes + 1, changes, changes - 1}
	for i, want := range wantVersions {
		if final[i].Version != want {
			t.Fatalf("final History()[%d].Version = %d, want %d", i, final[i].Version, want)
		}
	}
}

func versionsOf[T any](hist []mamori.Snapshot[T]) []uint64 {
	out := make([]uint64, len(hist))
	for i, s := range hist {
		out[i] = s.Version
	}
	return out
}
