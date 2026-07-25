package mamori

import "time"

// Snapshot is one validated configuration the Watcher applied, retained when
// WithHistory is enabled. Config is a full copy of T at that version, so
// enabling history extends the in-memory lifetime of any secret material it
// holds; that is why history defaults to off.
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot

	// applied is the cumulative per-field applied-version map as of this
	// snapshot (a copy, independent of the engine's live map). It is
	// unexported so it stays out of the public API; History() copies of
	// Snapshot carry it harmlessly since callers can neither observe nor
	// mutate it. Pin(version) uses it as the baseline for Unpin's coalesced
	// diff, so that pinning an older, already-retained snapshot diffs from
	// what was applied as of THAT snapshot rather than from whatever the
	// engine's live applied map happens to hold at Pin time.
	applied map[string]string
}

// History returns the retained snapshots, newest first, always including the
// current one. With WithHistory(0) (the default) only the current snapshot is
// returned. It is lock-free: it only ever Load()s the pointer most recently
// published by the reconciler goroutine and copies out of it, so a concurrent
// publish from recordSnapshot can never be observed mid-mutation.
func (w *Watcher[T]) History() []Snapshot[T] {
	p := w.snapshots.Load()
	if p == nil {
		return nil
	}
	src := *p
	out := make([]Snapshot[T], len(src))
	copy(out, src)
	return out
}
