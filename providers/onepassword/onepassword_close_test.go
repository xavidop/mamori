package onepassword

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
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

func onepasswordHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Query().Get("filter") != "":
		// The name-filter lookup finds nothing, so the provider falls back to
		// treating the path segment as an id directly.
		_, _ = w.Write([]byte(`[]`))
	case strings.HasSuffix(r.URL.Path, "/items"):
		_, _ = w.Write([]byte(`[]`))
	case strings.Contains(r.URL.Path, "/items/"):
		_, _ = w.Write([]byte(`{"id":"item","title":"item","version":1,"fields":[{"id":"f1","label":"password","value":"v"}]}`))
	default:
		_, _ = w.Write([]byte(`{"id":"vault","name":"vault"}`))
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
	srv := httptest.NewTLSServer(http.HandlerFunc(onepasswordHandler))
	t.Cleanup(srv.Close)
	client := srv.Client()

	p := New(WithHost(srv.URL), WithToken("test-token"), WithHTTPClient(client))
	ref, err := mamori.ParseRef("op://vault/item/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

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
	srv := httptest.NewTLSServer(http.HandlerFunc(onepasswordHandler))
	t.Cleanup(srv.Close)
	ct := &countingTransport{RoundTripper: srv.Client().Transport}
	client := &http.Client{Transport: ct}

	p := New(WithHost(srv.URL), WithToken("test-token"), WithHTTPClient(client))
	ref, err := mamori.ParseRef("op://vault/item/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

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

// TestCloseDoesNotEvictTheSharedDefaultTransport is the real-world regression
// this tier's nil-Transport guard exists for. New's own default client (no
// WithHTTPClient) is &http.Client{Timeout: ...} with no Transport set, and
// net/http resolves that nil Transport to the process-global
// http.DefaultTransport - the exact same transport http.DefaultClient uses.
//
// A service that resolves secrets from 1Password Connect and also calls some
// other API through anything built on http.DefaultClient (very common: any
// library that omits a Transport) shares that pool whether it knows it or
// not. If Close called CloseIdleConnections unconditionally, closing this
// provider on every config reload would silently evict that unrelated
// client's idle connections too, forcing its next call to pay a fresh TCP
// (and, over TLS, a handshake) it had no reason to expect. This test proves
// that does not happen: it warms http.DefaultClient's pool against a real
// server, closes an entirely unrelated, never-configured *Provider, and
// checks the pooled connection is still there afterward.
func TestCloseDoesNotEvictTheSharedDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	if err := New().Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !connReused(t, srv.URL) {
		t.Fatal("Close evicted the process-global http.DefaultTransport's idle connection pool; " +
			"a client whose Transport is nil must never have CloseIdleConnections called on it")
	}
}
