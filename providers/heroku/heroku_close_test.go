package heroku

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
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
// The server is TLS, not plaintext, and that is load-bearing for the first
// assertion, not incidental: srv.Client()'s transport carries the cert pool
// that trusts this server's self-signed certificate. Clearing that transport
// (the exact mutation this comment warns against) makes the request fall
// through to http.DefaultTransport, which does not trust that certificate,
// so the request would genuinely fail instead of quietly succeeding against
// plaintext 127.0.0.1 the way it would against an httptest.NewServer.
func TestCloseLeavesInjectedClientUsable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"LOG_LEVEL":"info"}`))
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()

	p := New(WithBaseURL(srv.URL), WithAPIKey("test-token"), WithApp("my-app"), WithHTTPClient(client))
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
		_, _ = w.Write([]byte(`{"LOG_LEVEL":"info"}`))
	}))
	t.Cleanup(srv.Close)
	ct := &countingTransport{RoundTripper: srv.Client().Transport}
	client := &http.Client{Transport: ct}

	p := New(WithBaseURL(srv.URL), WithAPIKey("test-token"), WithApp("my-app"), WithHTTPClient(client))
	ref := mustRef(t, "heroku://LOG_LEVEL")

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

// connReused issues one GET against target through http.DefaultClient and
// reports whether the round trip reused a pooled connection rather than
// dialing a new one.
func connReused(t *testing.T, target string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	var reused bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	return reused
}

// TestCloseDoesNotEvictTheSharedDefaultTransport pins the second half of the
// nil-Transport guard, the half New()'s own default never exercises: this
// provider builds no default client of its own (httpClient is nil unless
// WithHTTPClient is called), so the guard's `Transport != nil` clause has
// nothing to protect UNLESS a caller injects a client left at &http.Client{}
// - no Transport set, which net/http resolves to the process-global
// http.DefaultTransport, the exact transport http.DefaultClient uses.
//
// Before this test, removing that clause from Close left every suite in this
// module green: the field is nil in the common (never-injected) case, so
// nothing in TestCloseWithoutResolve or TestCloseLeavesInjectedClientUsable
// (which injects a client WITH a real Transport) would notice a missing
// Transport check. This test injects exactly the shape that check exists
// for, and would fail without it.
func TestCloseDoesNotEvictTheSharedDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	p := New(WithHTTPClient(&http.Client{}))
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !connReused(t, srv.URL) {
		t.Fatal("Close evicted the process-global http.DefaultTransport's idle connection pool; " +
			"an injected client whose Transport is nil must never have CloseIdleConnections called on it")
	}
}
