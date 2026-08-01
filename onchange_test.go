package mamori

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// onChangeOtherConfig is a second config type, declared at package scope so
// the type name in the mismatch error below is stable and worth asserting on.
// Mirrors preApplyOtherConfig in preapply_test.go for the identical reason.
type onChangeOtherConfig struct{ B string }

// TestOnChangeWrongTypeFailsWatch pins the fix for issue #111: OnChange[T]
// stores its callback in the non-generic options struct as any, and the
// assertion that used to recover it (reconciler.go, `onChange, _ =
// o.onChange.(func(Change[T]))`) discarded failure. A hook installed via
// OnChange[Foo] and passed to Watch[Bar] compiled, installed nothing, and the
// callback silently never fired - no error, no log, no Status signal. Watch
// must now refuse to start instead, the same way it already refuses a
// mismatched PreApply hook (TestPreApplyWrongTypeFailsWatch, preapply_test.go).
func TestOnChangeWrongTypeFailsWatch(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"oc://a"`
	}
	p := newWatchProvider("oc")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		OnChange(func(Change[onChangeOtherConfig]) {}),
	)
	if err == nil {
		_ = w.Close()
		t.Fatal("Watch accepted an OnChange hook typed for a different config: the callback would silently never fire")
	}
	if w != nil {
		t.Errorf("Watch returned a non-nil Watcher alongside its error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want one wrapping ErrInvalid", err)
	}
	// The message has to name both types: "OnChange hook has the wrong type" is
	// useless when the two candidates are two similarly-named config structs.
	for _, want := range []string{"OnChange", "onChangeOtherConfig", "cfg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestOnChangeCorrectTypeStillFires makes sure the added type check does not
// break the ordinary path: a hook installed for the same T Watch uses must
// still be invoked normally, with the applied Change delivered to it.
func TestOnChangeCorrectTypeStillFires(t *testing.T) {
	defer goleak.VerifyNone(t)

	clk := NewFakeClock(time.Time{})
	type cfg struct {
		A string `source:"ocg://a"`
	}
	p := newWatchProvider("ocg")
	p.set("a", "first", "v1")

	got := make(chan Change[cfg], 1)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p), WithClock(clk),
		OnChange(func(ev Change[cfg]) { got <- ev }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.push("a", "second", "v2")
	blockUntilTimers(t, clk, 1)
	clk.Advance(defaultDebounce)

	select {
	case ev := <-got:
		if ev.New.A != "second" {
			t.Errorf("New.A = %q, want second", ev.New.A)
		}
		if ev.Old.A != "first" {
			t.Errorf("Old.A = %q, want first", ev.Old.A)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for a correctly typed hook")
	}
	if got := w.Get().A; got != "second" {
		t.Errorf("Get().A = %q, want second", got)
	}
}

// TestOnChangeUnsetIsFine confirms installing no OnChange hook at all remains
// completely unaffected by the added type check: typedOnChange must return
// (nil, nil) when o.onChange is nil, exactly like typedPreApply does when no
// PreApply hook is installed, which is the common case for both.
func TestOnChangeUnsetIsFine(t *testing.T) {
	defer goleak.VerifyNone(t)

	type cfg struct {
		A string `source:"ocn://a"`
	}
	p := newWatchProvider("ocn")
	p.set("a", "first", "v1")

	w, err := Watch[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Watch with no OnChange installed: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().A; got != "first" {
		t.Errorf("Get().A = %q, want first", got)
	}
}
