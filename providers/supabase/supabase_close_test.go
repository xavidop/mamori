package supabase

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xavidop/mamori"
)

// countingTransport wraps a real RoundTripper and counts calls to
// CloseIdleConnections, so a test can prove Close actually reaches the
// client's pool rather than merely compiling a call that never fires - a
// typo'd field name, or a field that stays nil, would leave this counter at
// zero while every other assertion in this file still passed.
type countingTransport struct {
	http.RoundTripper
	closes int
}

func (c *countingTransport) CloseIdleConnections() {
	c.closes++
	if cc, ok := c.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		cc.CloseIdleConnections()
	}
}

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
//
// A real, TLS httptest.Server is used rather than fakeSupabase's in-process
// RoundTripper: the first assertion needs an actual round trip over the
// client's real transport, which an in-process fake has none of. TLS, not
// plaintext, is load bearing too: srv.Client()'s transport carries the cert
// pool that trusts this server's self-signed certificate, so clearing that
// transport (the exact mutation this comment warns against) makes the
// request fall through to http.DefaultTransport, which does not trust the
// certificate and genuinely fails - a plaintext server would let the request
// through regardless, against untrusted 127.0.0.1.
func TestCloseLeavesInjectedClientUsable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"K","decrypted_secret":"v","updated_at":"2026-01-01T00:00:00Z"}]`))
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()

	p := New(WithProjectURL(srv.URL), WithServiceKey(testServiceKey), WithHTTPClient(client))
	ref := mustRef(t, "supabase://K")

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

// TestCloseReleasesIdleConnections pins the other half of the Close contract
// this tier exists for: that Close actually reaches the tracked client's
// pool. Without this, deleting the CloseIdleConnections call from Close (or
// aiming it at a field that is always nil) leaves every other test in this
// file green, since none of them observes anything beyond "the request still
// works" and "Resolve now errors". The injected client here carries a real
// Transport, so it composes with (rather than being skipped by) Close's
// nil-Transport guard.
func TestCloseReleasesIdleConnections(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"K","decrypted_secret":"v","updated_at":"2026-01-01T00:00:00Z"}]`))
	}))
	t.Cleanup(srv.Close)
	ct := &countingTransport{RoundTripper: srv.Client().Transport}
	client := &http.Client{Transport: ct}

	p := New(WithProjectURL(srv.URL), WithServiceKey(testServiceKey), WithHTTPClient(client))
	ref := mustRef(t, "supabase://K")

	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ct.closes != 1 {
		t.Fatalf("CloseIdleConnections called %d times, want 1", ct.closes)
	}
}
