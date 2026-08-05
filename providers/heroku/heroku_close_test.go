package heroku

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xavidop/mamori"
)

// TestCloseWithoutResolve exercises the cold half of the Close contract
// directly: New followed by Close must not dial or panic, and Close must be
// idempotent. providertest's CloserContract case already covers this against
// a fresh instance from Config.New; this pins it here too, specifically
// against the plain New() a caller writes by hand.
func TestCloseWithoutResolve(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on a never-resolved provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseLeavesInjectedClientUsable is this tier's load-bearing Close case.
// CloseIdleConnections releases only idle connections and must never
// invalidate a client the caller (WithHTTPClient) still owns and uses
// elsewhere. A Close that instead cleared the client's Transport, or reached
// for some other teardown, would fail the first assertion below even though
// the second (Resolve reporting ErrUnavailable) still passed - the ordering
// bug providertest's generic CloserContract case cannot see, since it never
// injects a client of its own to check afterward.
func TestCloseLeavesInjectedClientUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"LOG_LEVEL":"info"}`))
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()

	p := New(WithBaseURL(srv.URL), WithAPIKey("test-token"), WithApp("my-app"), WithHTTPClient(client), WithAllowInsecure())
	ref := mustRef(t, "heroku://LOG_LEVEL")

	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Half 1: the injected client is still usable for a request the CALLER
	// makes directly, entirely outside the provider.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request through the injected client after Close: %v", err)
	}
	_ = resp.Body.Close()

	// Half 2: the provider itself is terminal.
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}
