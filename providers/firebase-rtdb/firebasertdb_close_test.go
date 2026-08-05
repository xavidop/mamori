package firebasertdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// TestCloseWithoutResolve exercises the cold half of the Close contract
// directly: New followed by Close must not dial or panic, and Close must be
// idempotent. providertest's CloserContract case already covers this against
// a fresh instance from Config.New; this pins it here too, specifically
// against the plain New() a caller writes by hand, which for this provider
// means no ADC lookup and no backend construction happen either.
func TestCloseWithoutResolve(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on a never-resolved provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseIsTerminal is this module's load-bearing Close case. Unlike its
// eleven siblings in this tier, firebase-rtdb exposes no WithHTTPClient: it
// holds no *http.Client of its own for Close to leave usable (see Close's
// doc comment), so there is no injected-client half to assert here. What
// remains, and what this test actually exercises, is that a provider which
// HAS resolved becomes terminal after Close: getBackend's closed check must
// run before its cached-backend check, or a backend built prior to Close
// would still answer a Resolve issued after it.
func TestCloseIsTerminal(t *testing.T) {
	fake := newFakeBackend()
	fake.set("config/k", `"v"`)

	p := New(withBackend(fake))
	ref := mustRef(t, "firebase-rtdb://config/k")

	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}
