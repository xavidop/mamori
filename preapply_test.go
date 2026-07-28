package mamori

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPreApplyOptionStoresTypedFunc(t *testing.T) {
	type cfg struct{ A string }
	o := defaultOptions()
	PreApply(func(context.Context, Change[cfg]) error { return nil })(o)
	if o.preApply == nil {
		t.Fatal("PreApply did not store the hook")
	}
	if _, ok := o.preApply.(func(context.Context, Change[cfg]) error); !ok {
		t.Fatalf("stored hook has type %T, want func(context.Context, Change[cfg]) error", o.preApply)
	}
}

func TestPreApplyTimeoutDefaultAndOverride(t *testing.T) {
	o := defaultOptions()
	if o.preApplyTimeout != defaultPreApplyTimeout {
		t.Errorf("default = %v, want %v", o.preApplyTimeout, defaultPreApplyTimeout)
	}
	WithPreApplyTimeout(3 * time.Second)(o)
	if o.preApplyTimeout != 3*time.Second {
		t.Errorf("after override = %v, want 3s", o.preApplyTimeout)
	}
}

func TestPreApplyErrorWrapsAndReports(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := &PreApplyError{Err: cause}
	if !errors.Is(err, cause) {
		t.Error("PreApplyError must unwrap to its cause")
	}
	if got := err.Error(); got == "" {
		t.Error("PreApplyError.Error() must not be empty")
	}
	var pe *PreApplyError
	if !errors.As(error(err), &pe) {
		t.Error("errors.As must reach *PreApplyError")
	}
}
