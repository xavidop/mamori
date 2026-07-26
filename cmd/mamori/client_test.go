package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

// oneField is the shared fixture config type for client.go's tests: a
// single required, unchained, non-sensitive field. Every test constructs
// its own mamoritest.Provider (scheme "ct") and Watcher, so reusing this
// same scheme/type across tests is safe: WithProvider is a per-call
// override, never the global registry, so nothing leaks between tests.
type oneField struct {
	Level string `source:"ct://level"`
}

// newHealthyWatcher builds a real *mamori.Watcher[oneField] whose single
// field has already resolved successfully, so w.Status().Healthy is true
// the moment Watch returns (the initial Load is synchronous).
func newHealthyWatcher(t *testing.T) *mamori.Watcher[oneField] {
	t.Helper()
	p := mamoritest.NewProvider("ct")
	p.Set("level", "info")
	w, err := mamori.Watch[oneField](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// waitForUnhealthy polls w.Status() until Healthy is false or the test
// times out, mirroring the waitUntil/waitFor helpers used elsewhere in this
// repo (notfound_health_test.go, mamoritest_test.go) for observing an async
// effect of a provider-side Fail landing on a real Watcher without a fixed
// sleep.
func waitForUnhealthy(t *testing.T, w *mamori.Watcher[oneField]) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !w.Status().Healthy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the watcher to report unhealthy")
}

// --- endpoint parsing -------------------------------------------------

func TestParseEndpointHTTPS(t *testing.T) {
	base, transport, err := parseEndpoint("https://example.com:8443", false)
	if err != nil {
		t.Fatalf("parseEndpoint() error = %v", err)
	}
	if base != "https://example.com:8443" {
		t.Errorf("base = %q, want https://example.com:8443", base)
	}
	if transport == nil {
		t.Fatal("transport = nil")
	}
}

func TestParseEndpointUnix(t *testing.T) {
	base, transport, err := parseEndpoint("unix:///tmp/mamori-client-test.sock", false)
	if err != nil {
		t.Fatalf("parseEndpoint() error = %v", err)
	}
	if base != "http://unix" {
		t.Errorf("base = %q, want http://unix", base)
	}
	if transport == nil || transport.DialContext == nil {
		t.Fatal("expected a transport with a custom DialContext for a unix endpoint")
	}
}

func TestParseEndpointHTTPInsecure(t *testing.T) {
	base, transport, err := parseEndpoint("http://127.0.0.1:9000", true)
	if err != nil {
		t.Fatalf("parseEndpoint() error = %v", err)
	}
	if base != "http://127.0.0.1:9000" {
		t.Errorf("base = %q, want http://127.0.0.1:9000", base)
	}
	if transport == nil {
		t.Fatal("transport = nil")
	}
}

func TestParseEndpointHTTPWithoutInsecureErrors(t *testing.T) {
	_, _, err := parseEndpoint("http://127.0.0.1:9000", false)
	if err == nil {
		t.Fatal("parseEndpoint() error = nil, want an error for plaintext http without --insecure")
	}
}

func TestParseEndpointUnsupportedScheme(t *testing.T) {
	_, _, err := parseEndpoint("ftp://example.com", false)
	if err == nil {
		t.Fatal("parseEndpoint() error = nil, want an error for an unsupported scheme")
	}
}

func TestParseEndpointEmpty(t *testing.T) {
	_, _, err := parseEndpoint("", false)
	if err == nil {
		t.Fatal("parseEndpoint() error = nil, want an error for an empty endpoint")
	}
}

// --- fetchReport exit-code classification ------------------------------

func TestFetchReportHealthyExit0(t *testing.T) {
	w := newHealthyWatcher(t)
	srv := httptest.NewServer(mamori.Handler(w))
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	rep, exit, err := fetchReport(context.Background(), f)
	if err != nil {
		t.Fatalf("fetchReport() error = %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if rep == nil || !rep.Healthy {
		t.Errorf("rep = %+v, want Healthy true", rep)
	}
}

func TestFetchReportUnhealthyExit1(t *testing.T) {
	p := mamoritest.NewProvider("ct")
	p.Set("level", "info")
	w, err := mamori.Watch[oneField](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// oneField.Level has no default and is not optional, so a terminal
	// not-found error surfaces as an unhealthy field rather than being
	// tolerated (see notfound_health_test.go for the same distinction in
	// core's own tests).
	p.Fail("level", mamori.ErrNotFound)
	waitForUnhealthy(t, w)

	srv := httptest.NewServer(mamori.Handler(w))
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	rep, exit, err := fetchReport(context.Background(), f)
	if err != nil {
		t.Fatalf("fetchReport() error = %v", err)
	}
	if exit != 1 {
		t.Errorf("exit = %d, want 1", exit)
	}
	if rep == nil || rep.Healthy {
		t.Errorf("rep = %+v, want Healthy false", rep)
	}
}

func TestFetchReportAdminOff404Exit2(t *testing.T) {
	// A bare mux with nothing registered: every path, including /, 404s.
	// This is what a process with no mamori.WithAdminHTTP (or an unrelated
	// service) looks like from the outside.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	rep, exit, err := fetchReport(context.Background(), f)
	if exit != 2 {
		t.Errorf("exit = %d, want 2", exit)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil", rep)
	}
	if err == nil || !strings.Contains(err.Error(), "WithAdminHTTP") {
		t.Errorf("err = %v, want a message pointing at WithAdminHTTP", err)
	}
}

func TestFetchReportNonReportBodyExit2(t *testing.T) {
	// A 200 whose body is valid JSON but not a mamori.Report: an unrelated
	// service that happens to also answer GET / with 200 application/json.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"hello":"world"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	_, exit, err := fetchReport(context.Background(), f)
	if exit != 2 {
		t.Errorf("exit = %d, want 2", exit)
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestFetchReportBareObjectExit2(t *testing.T) {
	// A bare {} decodes as a zero-value mamori.Report if decoded naively
	// (Healthy would be false, which would wrongly classify as exit 1). The
	// classifier must recognize this is not a real Report at all.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	_, exit, err := fetchReport(context.Background(), f)
	if exit != 2 {
		t.Errorf("exit = %d, want 2", exit)
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestFetchReportUnreachableClosedPortExit3(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("ln.Close: %v", err)
	}

	f := liveFlags{endpoint: "http://" + addr, insecure: true}
	rep, exit, err := fetchReport(context.Background(), f)
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil", rep)
	}
	if err == nil {
		t.Fatal("err = nil, want an error (connection refused)")
	}
}

func TestFetchReportUnreachableMissingSocketExit3(t *testing.T) {
	f := liveFlags{endpoint: "unix:///nonexistent/path/mamori-test-missing.sock"}
	rep, exit, err := fetchReport(context.Background(), f)
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil", rep)
	}
	if err == nil {
		t.Fatal("err = nil, want an error (no such socket)")
	}
}

func TestFetchReportBadEndpointExit3(t *testing.T) {
	// An endpoint that never even yields an HTTP request attempt (no
	// scheme parses, no client can be built) is classified the same as any
	// other "never got an HTTP response" outcome.
	f := liveFlags{endpoint: "ftp://example.com"}
	_, exit, err := fetchReport(context.Background(), f)
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestFetchReportUnauthorizedExit4(t *testing.T) {
	w := newHealthyWatcher(t)
	always401 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"error":"unauthorized"}`))
		})
	}
	h := mamori.Handler(w, mamori.HandlerMiddleware(always401))
	srv := httptest.NewServer(h)
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	rep, exit, err := fetchReport(context.Background(), f)
	if exit != 4 {
		t.Errorf("exit = %d, want 4", exit)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil", rep)
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}

func TestFetchReportUnexpectedStatusExit2(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := liveFlags{endpoint: srv.URL, insecure: true}
	_, exit, err := fetchReport(context.Background(), f)
	if exit != 2 {
		t.Errorf("exit = %d, want 2", exit)
	}
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
}
