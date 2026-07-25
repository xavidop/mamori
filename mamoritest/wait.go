// Package mamoritest (this file) adds deterministic wait/assertion helpers on
// top of the scriptable Provider in mamoritest.go: a test that pushes a change
// or a failure needs a way to block until that change has actually been
// applied (or that failure actually observed) by a real mamori.Watcher,
// without resorting to a fixed sleep that is either flaky (too short) or slow
// (too long).
package mamoritest

import (
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// defaultWait bounds how long the Wait helpers block before failing the test.
const defaultWait = 2 * time.Second

// WaitForSnapshot blocks until the watcher has applied snapshot version v, then
// returns. It fails the test if v is not reached within a bounded deadline. It
// is deterministic and does not sleep in the test body: set a value, then wait
// for the snapshot version to advance. The version contract this relies on
// (Watcher.Status().Snapshot starts at 1 for the initial load and increments
// by one per applied change, so the first change after Watch lands on 2) is
// owned by the reconciler, not by mamoritest.
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64) {
	tb.Helper()
	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		if w.Status().Snapshot >= v {
			return
		}
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("mamoritest: snapshot version %d not reached within %s (current %d)",
		v, defaultWait, w.Status().Snapshot)
}

// ErrorCapture records errors delivered to OnError for later assertion. It is
// safe for concurrent use: the reconciler delivers OnError callbacks from its
// own goroutine while a test's goroutine polls WaitForError.
type ErrorCapture struct {
	mu   sync.Mutex
	errs []error
}

// CaptureErrors returns an Option installing an OnError sink plus the capture it
// feeds. Pass the Option to Watch (or Load) and assert against the capture
// with WaitForError.
func CaptureErrors() (mamori.Option, *ErrorCapture) {
	c := &ErrorCapture{}
	opt := mamori.OnError(func(err error) {
		c.mu.Lock()
		c.errs = append(c.errs, err)
		c.mu.Unlock()
	})
	return opt, c
}

// WaitForError blocks until the capture holds an error classified as kind, then
// returns it. It fails the test if no such error arrives within the deadline.
func WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error {
	tb.Helper()
	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, err := range c.errs {
			if mamori.ErrorKind(err) == kind {
				c.mu.Unlock()
				return err
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("mamoritest: no error of kind %q within %s", kind, defaultWait)
	return nil
}
