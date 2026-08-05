package firebaserc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
)

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
// this provider's nil-Transport guard exists for. WithHTTPClient stores the
// caller's client verbatim in p.httpClient, and a client left with no
// Transport set - &http.Client{} - has net/http resolve that nil Transport to
// the process-global http.DefaultTransport, the exact same transport
// http.DefaultClient uses.
//
// A service that resolves Remote Config parameters here and also calls some
// other API through anything built on http.DefaultClient (very common: any
// library that omits a Transport) shares that pool whether it knows it or
// not. If Close called CloseIdleConnections unconditionally, closing this
// provider on every config reload would silently evict that unrelated
// client's idle connections too, forcing its next call to pay a fresh TCP
// (and, over TLS, a handshake) it had no reason to expect. This test proves
// that does not happen: it warms http.DefaultClient's pool against a real
// server, resolves once (through the SAME Transport-less client, so the
// fetcher buildFetcher builds is the one Close later touches) and closes,
// then checks the pooled connection is still there afterward.
//
// WithFetcher is deliberately NOT used here: it would bypass buildFetcher
// entirely and leave p.fetcher holding a fake rather than the *httpFetcher
// Close's type assertion looks for, which would make this test pass
// vacuously regardless of the guard. Resolve's own outcome is irrelevant -
// the test server answers with an empty 200, which fails to decode as a
// template - what matters is only that getFetcher has already cached the
// real fetcher (built and stored before the network round trip happens).
func TestCloseDoesNotEvictTheSharedDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	p := New(WithProjectID("demo"), WithBaseURL(srv.URL), WithHTTPClient(&http.Client{}))
	_, _ = p.Resolve(context.Background(), parse(t, "firebase-rc://welcome_message"))

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !connReused(t, srv.URL) {
		t.Fatal("Close evicted the process-global http.DefaultTransport's idle connection pool; " +
			"a client whose Transport is nil must never have CloseIdleConnections called on it")
	}
}
