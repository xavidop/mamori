package mamori

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// These tests exercise the runtime counterpart of Doctor's default/optional
// not-found tolerance (doctor.go). A native-watch provider (unlike the poll
// path, which already swallows ErrNotFound silently) can otherwise deliver
// ErrNotFound straight into handleErr, which must tolerate it exactly the
// same way for a field with a default or marked optional, while a required
// field must still be recorded and surfaced as unhealthy.

// waitUntil polls cond every 5ms until it returns true or the deadline
// elapses, failing the test if the deadline is reached first.
func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func TestHandleErrToleratesNotFoundOnDefaultField(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("nftol1")
	wp.set("cfg/level", "info", "l1")

	type Config struct {
		Level string `source:"nftol1://cfg/level" default:"fallback"`
	}

	w, err := Watch[Config](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	if err := w.Health(); err != nil {
		t.Fatalf("initial Health = %v, want nil", err)
	}

	// The provider's backing key disappears at runtime; the native watch
	// delivers ErrNotFound directly (this is what providers/k8s, firestore and
	// redis do on a delete/expiry event).
	wp.pushErr("cfg/level", ErrNotFound)

	// Give the reconciler goroutine time to consume the update. There is no
	// externally observable state change to poll for in the tolerated case
	// (that is the point of the fix): the assertion below is that nothing
	// happened, and a tolerated not-found arms no timer and moves no version,
	// so there is nothing for blockUntilTimers or waitFlushed to wait on. A
	// fixed delay is the honest shape for a negative assertion - unlike an
	// Advance, it can only make this test miss a regression, never invent a
	// failure.
	time.Sleep(50 * time.Millisecond)

	rep := w.Status()
	if !rep.Healthy {
		t.Fatalf("Status().Healthy = false after tolerated not-found on a default field: %+v", rep)
	}
	if len(rep.Fields) != 1 {
		t.Fatalf("Status reported %d fields, want 1", len(rep.Fields))
	}
	f := rep.Fields[0]
	if f.LastError != "" || f.LastKind != "" {
		t.Fatalf("field carries an error after a tolerated not-found: kind=%q err=%q", f.LastKind, f.LastError)
	}
	if err := w.Health(); err != nil {
		t.Fatalf("Health() = %v after tolerated not-found on a default field, want nil", err)
	}
}

func TestHandleErrClearsPriorErrorOnToleratedNotFound(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("nftol2")
	wp.set("cfg/level", "info", "l1")

	type Config struct {
		Level string `source:"nftol2://cfg/level" default:"fallback"`
	}

	w, err := Watch[Config](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// First, a genuine (non-not-found) error makes the field unhealthy: a
	// default does not tolerate a permission-denied, only an absence.
	wp.pushErr("cfg/level", fmt.Errorf("%w: denied", ErrPermissionDenied))
	waitUntil(t, 2*time.Second, "field to become unhealthy on permission-denied", func() bool {
		rep := w.Status()
		return len(rep.Fields) == 1 && rep.Fields[0].LastKind == KindPermissionDenied
	})
	if err := w.Health(); err == nil {
		t.Fatalf("Health() = nil after a permission-denied error, want non-nil")
	}

	// The provider's key is then deleted (ErrNotFound). Since this field has a
	// default, that must be tolerated and clear the prior error, restoring
	// health, exactly as Doctor already reports for the same field/situation.
	wp.pushErr("cfg/level", ErrNotFound)
	waitUntil(t, 2*time.Second, "field to become healthy again after tolerated not-found", func() bool {
		return w.Health() == nil
	})

	rep := w.Status()
	if !rep.Healthy {
		t.Fatalf("Status().Healthy = false after tolerated not-found cleared the prior error: %+v", rep)
	}
	f := rep.Fields[0]
	if f.LastError != "" || f.LastKind != "" {
		t.Fatalf("field still carries the prior error after a tolerated not-found: kind=%q err=%q", f.LastKind, f.LastError)
	}
}

func TestHandleErrRequiredFieldBecomesUnhealthyOnNotFound(t *testing.T) {
	defer goleak.VerifyNone(t)
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("nfreq")
	wp.set("cfg/level", "info", "l1")

	// No default and not optional: this field is required, so an absent value
	// at runtime is a genuine problem, not something to tolerate.
	type Config struct {
		Level string `source:"nfreq://cfg/level"`
	}

	w, err := Watch[Config](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	if err := w.Health(); err != nil {
		t.Fatalf("initial Health = %v, want nil", err)
	}

	wp.pushErr("cfg/level", ErrNotFound)
	waitUntil(t, 2*time.Second, "required field to record KindNotFound", func() bool {
		rep := w.Status()
		return len(rep.Fields) == 1 && rep.Fields[0].LastKind == KindNotFound
	})

	rep := w.Status()
	if rep.Healthy {
		t.Fatalf("Status().Healthy = true after a required field went not-found, want false: %+v", rep)
	}
	f := rep.Fields[0]
	if f.LastError == "" {
		t.Fatalf("required field has no LastError after going not-found")
	}

	err = w.Health()
	if err == nil {
		t.Fatalf("Health() = nil after a required field went not-found, want a *HealthError")
	}
	var he *HealthError
	if !errors.As(err, &he) {
		t.Fatalf("Health() error = %T, want *HealthError", err)
	}
	if len(he.Fields) != 1 || he.Fields[0].Path != "Level" {
		t.Fatalf("HealthError.Fields = %+v, want [Level]", he.Fields)
	}
}
