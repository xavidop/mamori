package mamori

import (
	"context"
	"testing"
)

type stubProvider struct {
	scheme string
	val    Value
}

func (s *stubProvider) Scheme() string { return s.scheme }
func (s *stubProvider) Resolve(_ context.Context, _ Ref) (Value, error) {
	return s.val, nil
}

func TestRegisterAndLookup(t *testing.T) {
	unregister("stubx")
	Register(&stubProvider{scheme: "stubx"})
	t.Cleanup(func() { unregister("stubx") })

	p, ok := providerFor("stubx")
	if !ok {
		t.Fatal("providerFor(stubx) not found after Register")
	}
	if p.Scheme() != "stubx" {
		t.Fatalf("scheme = %q, want stubx", p.Scheme())
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	unregister("stuby")
	Register(&stubProvider{scheme: "stuby"})
	t.Cleanup(func() { unregister("stuby") })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("second Register of same scheme did not panic")
		}
	}()
	Register(&stubProvider{scheme: "stuby"})
}

func TestRegisterNilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register(nil) did not panic")
		}
	}()
	Register(nil)
}
