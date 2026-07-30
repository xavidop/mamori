package secret

import "testing"

// TestZeroAliasesEveryCopy pins the hazard Clone exists to solve, and is the
// reason mamori never zeroes a secret itself.
//
// A String is a struct holding a slice, so copying one shares the backing
// array. Watcher.Get returns config by value, so a caller's "own" copy reads
// through to the same bytes the reconciler serves. This test documents that
// truthfully rather than asserting the comfortable thing, so that a later
// change which makes Zero look safe fails here instead of in production.
func TestZeroAliasesEveryCopy(t *testing.T) {
	orig := NewString("hunter2")
	shared := orig // what Watcher.Get hands a caller

	orig.Zero()

	if got := shared.Reveal(); got == "hunter2" {
		t.Fatal("copies no longer alias; Clone and the Zero doc comment need revisiting")
	}
}

func TestCloneBreaksAliasing(t *testing.T) {
	orig := NewString("hunter2")
	owned := orig.Clone()

	owned.Zero()

	if got := orig.Reveal(); got != "hunter2" {
		t.Errorf("zeroing a clone corrupted the original: %q", got)
	}
	if !owned.IsZero() {
		t.Error("the clone was not zeroed")
	}
}

func TestCloneRevealsSameValue(t *testing.T) {
	orig := NewString("hunter2")
	if got := orig.Clone().Reveal(); got != "hunter2" {
		t.Errorf("Clone().Reveal() = %q, want %q", got, "hunter2")
	}
}

func TestCloneOfEmptyDoesNotAllocate(t *testing.T) {
	var s String
	c := s.Clone()
	if !c.IsZero() {
		t.Error("cloning an empty String produced a non-empty one")
	}
	if n := testing.AllocsPerRun(100, func() { _ = s.Clone() }); n != 0 {
		t.Errorf("cloning an empty String allocated %v times, want 0", n)
	}
}

func TestBytesCloneBreaksAliasing(t *testing.T) {
	orig := NewBytes([]byte("hunter2"))
	owned := orig.Clone()

	owned.Zero()

	if got := string(orig.Reveal()); got != "hunter2" {
		t.Errorf("zeroing a clone corrupted the original: %q", got)
	}
	if !owned.IsZero() {
		t.Error("the clone was not zeroed")
	}
}

// TestCloneIsRedactedToo guards against the obvious mistake of building Clone
// in a way that loses the redaction contract.
func TestCloneIsRedactedToo(t *testing.T) {
	c := NewString("hunter2").Clone()
	if c.String() != Redacted {
		t.Errorf("Clone().String() = %q, want %q", c.String(), Redacted)
	}
	b := NewBytes([]byte("hunter2")).Clone()
	if b.String() != Redacted {
		t.Errorf("Bytes.Clone().String() = %q, want %q", b.String(), Redacted)
	}
}
